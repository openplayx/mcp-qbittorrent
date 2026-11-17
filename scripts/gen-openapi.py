#!/usr/bin/env python3
"""Generate src/api/openapi.json (OpenAPI 3.1). Run after changing the HTTP API;
src/internal/api tests check that every served route is documented."""
import json
import os


def ref(n):
    return {"$ref": f"#/components/schemas/{n}"}


def jbody(n, required=True):
    return {"required": required, "content": {"application/json": {"schema": ref(n)}}}


def ok(desc, n=None):
    r = {"description": desc}
    if n:
        r["content"] = {"application/json": {"schema": ref(n)}}
    return r


E = {"$ref": "#/components/responses/Error"}


def errs(*codes):
    return {str(c): E for c in codes}


def S(**kw):
    return {"type": "object", **kw}


ts = {"type": "string", "format": "date-time"}
removed = {"type": "object", "description": "Nested addresses dropped by the network policy (the input is kept)",
           "properties": {"private_or_local_address": {"type": "integer"}, "unsupported_address": {"type": "integer"}}}
idp = [{"name": "id", "in": "path", "required": True, "schema": {"type": "string", "pattern": "^[0-9a-f]{40}$"},
        "description": "Engine torrent hash (v1 info hash, or the 40-hex id of a v2-only torrent)."}]
fid = {"name": "file_id", "in": "path", "required": True, "schema": {"type": "string", "pattern": "^f[0-9]+$"}}
states = ["metadata_pending", "queued", "downloading", "paused", "checking", "stalled", "completed", "seeding", "error"]

paths = {
    "/api/healthz": {"get": {"summary": "Process liveness", "security": [], "responses": {"200": ok("Alive", "Health")}}},
    "/api/readyz": {"get": {"summary": "Engine reachable (probed live)", "security": [],
                            "responses": {"200": ok("Ready", "Ready"), "503": ok("Not ready", "Ready")}}},
    "/api/openapi.json": {"get": {"summary": "This document", "security": [], "responses": {"200": {"description": "OpenAPI 3.1"}}}},
    "/api/docs": {"get": {"summary": "Human-readable API docs", "security": [], "responses": {"200": {"description": "HTML"}}}},
    "/api/v1/capabilities": {"get": {"summary": "Engine version, inputs, limits, destinations",
                                     "responses": {"200": ok("Capabilities", "Capabilities"), **errs(401, 429)}}},
    "/api/v1/destinations": {"get": {"summary": "Logical destinations with free space",
                                     "responses": {"200": ok("Destinations", "Destinations"), **errs(401, 429)}}},
    "/api/v1/torrent-inputs": {"post": {
        "summary": "Upload a size-limited .torrent; kept in memory briefly, returns input_ref",
        "requestBody": {"required": True, "content": {
            "application/x-bittorrent": {"schema": {"type": "string", "contentMediaType": "application/x-bittorrent"}},
            "multipart/form-data": {"schema": S(properties={"torrent": {"type": "string", "contentMediaType": "application/x-bittorrent"}},
                                                required=["torrent"])}}},
        "responses": {"201": ok("Stored", "Input"), **errs(400, 401, 413, 415, 422, 429)}}},
    "/api/v1/torrent-plans": {"post": {
        "summary": "Validate a magnet or input_ref, destination and strategy; returns a signed, expiring plan_ref. Contacts no peers.",
        "requestBody": jbody("PlanRequest"), "responses": {"201": ok("Plan", "Plan"), **errs(400, 401, 409, 415, 422, 429, 503)}}},
    "/api/v1/torrents": {
        "post": {"summary": "Add the planned torrent; returns once the engine lists it. Repeating returns the existing torrent (200).",
                 "requestBody": jbody("CreateRequest"),
                 "responses": {"201": ok("Created", "Task"), "200": ok("Already in the engine", "Task"),
                               **errs(400, 401, 409, 415, 422, 429, 503, 504)}},
        "get": {"summary": "All torrents in the engine, read live (including ones added in the WebUI)",
                "responses": {"200": ok("List", "TaskList"), **errs(401, 429, 503)}}},
    "/api/v1/torrents/{id}": {"get": {"summary": "One torrent, read live", "parameters": idp,
                                      "responses": {"200": ok("Task", "Task"), **errs(401, 404, 429, 503)}}},
    "/api/v1/torrents/{id}/files": {"get": {"summary": "Files once metadata is known, with the selection revision", "parameters": idp,
                                            "responses": {"200": ok("Files", "Files"), **errs(401, 404, 429, 503)}}},
    "/api/v1/torrents/{id}/selection": {"patch": {
        "summary": "Change the file selection against expected_revision", "parameters": idp, "requestBody": jbody("SelectionRequest"),
        "responses": {"200": ok("Files after the change", "Files"), **errs(400, 401, 404, 409, 415, 422, 429, 503, 504)}}},
    "/api/v1/torrents/{id}/pause": {"post": {"summary": "Stop; returns once the engine reports it stopped", "parameters": idp,
                                             "responses": {"200": ok("Task", "Task"), **errs(401, 404, 429, 503, 504)}}},
    "/api/v1/torrents/{id}/resume": {"post": {"summary": "Start (never force-start); quota and disk holds are re-checked", "parameters": idp,
                                              "responses": {"200": ok("Task", "Task"), **errs(401, 404, 409, 429, 503, 504)}}},
    "/api/v1/torrents/{id}/limits": {"patch": {
        "summary": "Per-torrent rates within administrator ceilings", "parameters": idp, "requestBody": jbody("LimitsRequest"),
        "responses": {"200": ok("Task", "Task"), **errs(400, 401, 404, 422, 429, 503, 504)}}},
    "/api/v1/torrents/{id}/remove-plan": {"post": {
        "summary": "Preview removal; returns a signed plan_ref (files kept unless delete_files)", "parameters": idp,
        "requestBody": jbody("RemovePlanRequest", False),
        "responses": {"201": ok("Remove plan", "RemovePlan"), **errs(401, 404, 429, 503)}}},
    "/api/v1/torrents/{id}/remove": {"post": {
        "summary": "Apply a remove plan", "parameters": idp, "requestBody": jbody("RemoveRequest"),
        "responses": {"200": ok("Removed", "RemoveResult"), **errs(400, 401, 409, 422, 429, 503, 504)}}},
    "/api/v1/torrents/{id}/files/{file_id}/content": {
        "get": {"summary": "Read a complete file (Range supported). Only when capabilities.file_content is true.",
                "parameters": idp + [fid, {"name": "Range", "in": "header", "schema": {"type": "string"}}],
                "responses": {"200": {"description": "File bytes"}, "206": {"description": "Partial content"},
                              "416": {"description": "Range not satisfiable"}, **errs(401, 403, 404, 409, 429)}},
        "head": {"summary": "Size and type of a complete file", "parameters": idp + [fid],
                 "responses": {"200": {"description": "Headers only"}, **errs(401, 403, 404, 409, 429)}}},
}

schemas = {
    "Error": S(required=["error"], properties={"error": S(required=["code", "request_id", "retryable"], properties={
        "code": {"type": "string"}, "request_id": {"type": "string"}, "retryable": {"type": "boolean"}, "detail": {"type": "object"}})}),
    "Health": S(properties={"status": {"type": "string"}, "version": {"type": "string"}}),
    "Ready": S(properties={"ready": {"type": "boolean"}, "engine": {"type": "boolean"}}),
    "Destination": S(properties={"destination_ref": {"type": "string"}, "name": {"type": "string"},
                                 "free_bytes": {"type": "integer"}, "max_task_bytes": {"type": "integer"}}),
    "Destinations": S(properties={"destinations": {"type": "array", "items": ref("Destination")}}),
    "Capabilities": S(properties={
        "service": {"type": "object"}, "engine": {"type": "object"},
        "inputs": {"type": "array", "items": {"type": "string", "enum": ["magnet", "torrent_file"]}},
        "torrent_versions": {"type": "array", "items": {"type": "string"}},
        "selection_presets": {"type": "array", "items": {"type": "string"}},
        "operations": {"type": "array", "items": {"type": "string"}},
        "state_storage": {"type": "string", "enum": ["none"]}, "all_engine_torrents": {"type": "boolean"},
        "file_content": {"type": "boolean"}, "webui": {"type": "boolean"}, "streaming_while_downloading": {"type": "boolean"},
        "metadata_only_guarantee": {"type": "boolean", "description": "false: a magnet may transfer a little payload data before it is stopped"},
        "limits": {"type": "object"}, "seeding": {"type": "object"},
        "destinations": {"type": "array", "items": ref("Destination")}}),
    "Input": S(properties={"input_ref": {"type": "string"}, "name": {"type": "string"},
                           "torrent_version": {"type": "string", "enum": ["v1", "v2", "hybrid"]},
                           "file_count": {"type": "integer"}, "total_size": {"type": "integer"},
                           "private": {"type": "boolean"}, "removed_addresses": removed, "expires_at": ts}),
    "PlanRequest": S(required=["source", "destination_ref"], additionalProperties=False, properties={
        "source": S(required=["type"], additionalProperties=False, properties={
            "type": {"type": "string", "enum": ["magnet", "input"]},
            "magnet": {"type": "string", "pattern": "^magnet:\\?"}, "input_ref": {"type": "string"}}),
        "destination_ref": {"type": "string"},
        "selection_preset": {"type": "string", "enum": ["all", "video"], "default": "all"},
        "auto_start": {"type": "boolean", "default": True},
        "download_limit": {"type": "integer", "minimum": 0, "description": "bytes/s, 0 = administrator ceiling"},
        "upload_limit": {"type": "integer", "minimum": 0}}),
    "Plan": S(properties={
        "plan_ref": {"type": "string", "description": "Opaque signed token; valid until expires_at or a server restart"},
        "expires_at": ts, "input_kind": {"type": "string"}, "name": {"type": "string"}, "torrent_version": {"type": "string"},
        "metadata": {"type": "string", "enum": ["known", "unknown"]}, "total_size": {"type": "integer"},
        "selected_size": {"type": "integer"}, "file_count": {"type": "integer"},
        "files_preview": {"type": "array", "items": S(properties={"path": {"type": "string"}, "size": {"type": "integer"},
                                                                  "selected": {"type": "boolean"}})},
        "destination": ref("Destination"), "selection_preset": {"type": "string"}, "auto_start": {"type": "boolean"},
        "download_limit": {"type": "integer"}, "upload_limit": {"type": "integer"},
        "trackers": {"type": "integer", "description": "trackers kept after filtering"},
        "removed_addresses": removed,
        "warnings": {"type": "array", "items": {"type": "string"},
                     "description": "e.g. metadata_unknown, addresses_removed, no_trackers_dht_only, private_torrent"}}),
    "CreateRequest": S(required=["plan_ref"], additionalProperties=False, properties={"plan_ref": {"type": "string"}}),
    "Task": S(properties={
        "id": {"type": "string"}, "name": {"type": "string"}, "state": {"type": "string", "enum": states},
        "hold_reason": {"type": "string", "enum": ["awaiting_start", "quota_exceeded", "disk_low", "no_files_match_preset"]},
        "error_code": {"type": "string"}, "torrent_version": {"type": "string"}, "destination_ref": {"type": "string"},
        "added_via": {"type": "string", "enum": ["app", "other"]},
        "progress": {"type": "number", "description": "0..1 over selected files"},
        "selected_size": {"type": "integer"}, "total_size": {"type": "integer"}, "completed_bytes": {"type": "integer"},
        "downloaded_bytes": {"type": "integer"}, "uploaded_bytes": {"type": "integer"},
        "download_speed": {"type": "integer"}, "upload_speed": {"type": "integer"},
        "download_limit": {"type": "integer"}, "upload_limit": {"type": "integer"},
        "seeds": {"type": "integer"}, "peers": {"type": "integer"}, "ratio": {"type": "number"}, "eta_seconds": {"type": "integer"},
        "metadata_known": {"type": "boolean"}, "payload_complete": {"type": "boolean"},
        "selected_files_complete": {"type": "boolean"}, "seeding": {"type": "boolean"},
        "added_at": ts, "completed_at": ts}),
    "TaskList": S(properties={"items": {"type": "array", "items": ref("Task")}}),
    "File": S(properties={
        "file_id": {"type": "string"}, "path": {"type": "string", "description": "Relative to the torrent, never an absolute path"},
        "size": {"type": "integer"}, "progress": {"type": "number"}, "selected": {"type": "boolean"},
        "complete": {"type": "boolean"}, "is_video": {"type": "boolean"}, "content_available": {"type": "boolean"}}),
    "Files": S(properties={"task_id": {"type": "string"}, "metadata_known": {"type": "boolean"},
                           "files_revision": {"type": "string"}, "files": {"type": "array", "items": ref("File")}}),
    "SelectionRequest": S(required=["expected_revision"], additionalProperties=False,
                          description="Exactly one of preset or file_ids.", properties={
                              "expected_revision": {"type": "string"}, "preset": {"type": "string", "enum": ["all", "video"]},
                              "file_ids": {"type": "array", "items": {"type": "string"}, "maxItems": 20000}}),
    "LimitsRequest": S(additionalProperties=False, properties={"download_limit": {"type": "integer", "minimum": 0},
                                                              "upload_limit": {"type": "integer", "minimum": 0}}),
    "RemovePlanRequest": S(additionalProperties=False, properties={"delete_files": {"type": "boolean", "default": False}}),
    "RemovePlan": S(properties={
        "plan_ref": {"type": "string"}, "task_id": {"type": "string"}, "task_name": {"type": "string"},
        "delete_files": {"type": "boolean"}, "files_on_disk": {"type": "integer"}, "bytes_on_disk": {"type": "integer"},
        "added_via": {"type": "string"}, "stops_seeding": {"type": "boolean"},
        "effects": {"type": "array", "items": {"type": "string"}}, "expires_at": ts}),
    "RemoveRequest": S(required=["plan_ref"], additionalProperties=False, properties={"plan_ref": {"type": "string"}}),
    "RemoveResult": S(properties={"task_id": {"type": "string"}, "removed": {"type": "boolean"}, "data_deleted": {"type": "boolean"}}),
}

doc = {
    "openapi": "3.1.0",
    "info": {
        "title": "OpenPlayX BitTorrent API", "version": "2.0.0", "license": {"name": "GNU GPL v3 (see COPYING)"},
        "description": (
            "Stateless HTTP API over a pinned qBittorrent engine: every read is live from the engine and all engine torrents "
            "(including WebUI-added ones) are listed and controllable. Plans and removal confirmations are signed tokens. "
            "Service endpoints live under /api/ (except /api/v2/, the engine WebUI's own API); other paths serve the engine "
            "WebUI through a guarded proxy when enabled. Engine credentials, SIDs, full magnets and absolute paths are never returned."),
    },
    "security": [{"bearer": []}],
    "paths": paths,
    "components": {
        "securitySchemes": {"bearer": {"type": "http", "scheme": "bearer"}},
        "responses": {"Error": {"description": "Error with stable code, request ID and retryability",
                                "content": {"application/json": {"schema": ref("Error")}}}},
        "schemas": schemas,
    },
}

out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src", "api", "openapi.json")
with open(out, "w") as f:
    f.write(json.dumps(doc, indent=2, ensure_ascii=False) + "\n")
