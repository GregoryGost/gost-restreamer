-- +goose Up

CREATE TABLE storage_format (singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL);
INSERT INTO storage_format(singleton,version) VALUES(1,2);

CREATE TABLE fallback_assets (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    incarnation TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('PREPARING','READY','FAILED')),
    current_generation_id TEXT,
    pending_generation_id TEXT,
    FOREIGN KEY(current_generation_id,id) REFERENCES fallback_generations(id,asset_id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(pending_generation_id,id) REFERENCES fallback_generations(id,asset_id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    CHECK(current_generation_id IS NULL OR current_generation_id IS NOT pending_generation_id)
);

CREATE TABLE fallback_generations (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    incarnation TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('QUEUED','CLAIMED','ISSUED','PUBLISHED','FAILED','INTERRUPTED')),
    issued INTEGER NOT NULL CHECK(issued IN (0,1)),
    cleanup_pending INTEGER NOT NULL CHECK(cleanup_pending IN (0,1)),
    source_hash TEXT NOT NULL,
    UNIQUE(id,asset_id),
    UNIQUE(id,asset_id,source_hash)
);

CREATE TABLE streams (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    publication_key TEXT NOT NULL UNIQUE,
    asset_id TEXT REFERENCES fallback_assets(id) ON DELETE RESTRICT
);

CREATE TABLE profiles (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL
);

CREATE TABLE groups (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    UNIQUE(id,stream_id),
    UNIQUE(id,stream_id,profile_id)
);

CREATE TABLE destinations (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    group_id TEXT NOT NULL,
    FOREIGN KEY(group_id,stream_id) REFERENCES groups(id,stream_id) ON DELETE RESTRICT,
    UNIQUE(id,stream_id,group_id)
);

CREATE TABLE fallback_variants (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    generation_id TEXT NOT NULL,
    incarnation TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    profile_key TEXT NOT NULL,
    canonical_profile BLOB NOT NULL,
    generator_version TEXT NOT NULL,
    managed_path TEXT NOT NULL UNIQUE,
    size_bytes TEXT COLLATE BINARY NOT NULL CHECK(length(size_bytes)=20 AND size_bytes NOT GLOB '*[^0-9]*' AND size_bytes<='18446744073709551615'),
    state TEXT NOT NULL CHECK(state IN ('PREPARING','READY','FAILED')),
    UNIQUE(id,asset_id,generation_id),
    UNIQUE(generation_id,canonical_profile),
    FOREIGN KEY(managed_path,generation_id,asset_id) REFERENCES owned_paths(managed_path,generation_id,asset_id) ON DELETE RESTRICT,
    FOREIGN KEY(generation_id,asset_id,source_hash) REFERENCES fallback_generations(id,asset_id,source_hash) ON DELETE RESTRICT
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    stream_id TEXT NOT NULL,
    incarnation TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('STARTING','RUNNING','STOPPING','STOPPED','FAILED','INTERRUPTED')),
    lifecycle TEXT COLLATE BINARY NOT NULL CHECK(length(lifecycle)=20 AND lifecycle NOT GLOB '*[^0-9]*' AND lifecycle<='18446744073709551615'),
    issued INTEGER NOT NULL CHECK(issued IN (0,1)),
    cleanup_pending INTEGER NOT NULL CHECK(cleanup_pending IN (0,1))
);

CREATE TABLE outputs (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    generation TEXT COLLATE BINARY NOT NULL CHECK(length(generation)=20 AND generation NOT GLOB '*[^0-9]*' AND generation<='18446744073709551615'),
    attempt TEXT COLLATE BINARY NOT NULL CHECK(length(attempt)=20 AND attempt NOT GLOB '*[^0-9]*' AND attempt<='18446744073709551615'),
    issued INTEGER NOT NULL CHECK(issued IN (0,1)),
    cleanup_pending INTEGER NOT NULL CHECK(cleanup_pending IN (0,1)),
    UNIQUE(id,session_id)
);

CREATE TABLE command_receipts (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    request_id TEXT NOT NULL UNIQUE,
    session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT,
    output_id TEXT,
    incarnation TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('PENDING','CLAIMED','ISSUED','SUCCEEDED','FAILED','INTERRUPTED')),
    lifecycle TEXT COLLATE BINARY NOT NULL CHECK(length(lifecycle)=20 AND lifecycle NOT GLOB '*[^0-9]*' AND lifecycle<='18446744073709551615'),
    generation TEXT COLLATE BINARY NOT NULL CHECK(length(generation)=20 AND generation NOT GLOB '*[^0-9]*' AND generation<='18446744073709551615'),
    attempt TEXT COLLATE BINARY NOT NULL CHECK(length(attempt)=20 AND attempt NOT GLOB '*[^0-9]*' AND attempt<='18446744073709551615'),
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT
);

CREATE TABLE cleanup_intents (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('runtime','file')),
    stream_id TEXT,
    session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT,
    output_id TEXT,
    incarnation TEXT NOT NULL,
    lifecycle TEXT COLLATE BINARY NOT NULL CHECK(length(lifecycle)=20 AND lifecycle NOT GLOB '*[^0-9]*' AND lifecycle<='18446744073709551615'),
    generation TEXT COLLATE BINARY NOT NULL CHECK(length(generation)=20 AND generation NOT GLOB '*[^0-9]*' AND generation<='18446744073709551615'),
    attempt TEXT COLLATE BINARY NOT NULL CHECK(length(attempt)=20 AND attempt NOT GLOB '*[^0-9]*' AND attempt<='18446744073709551615'),
    managed_path TEXT REFERENCES owned_paths(managed_path) ON DELETE RESTRICT,
    artifact_generation_id TEXT,
    file_id TEXT REFERENCES owned_paths(id) ON DELETE RESTRICT,
    acknowledged INTEGER NOT NULL CHECK(acknowledged IN (0,1)),
    FOREIGN KEY(file_id,managed_path,artifact_generation_id) REFERENCES owned_paths(id,managed_path,generation_id) ON DELETE RESTRICT,
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT,
    CHECK((kind='file' AND managed_path IS NOT NULL) OR (kind='runtime' AND session_id IS NOT NULL))
);

CREATE TABLE owned_paths (
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    managed_path TEXT NOT NULL UNIQUE,
    asset_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('directory','original','variant','staging')),
    UNIQUE(id,managed_path,generation_id),
    UNIQUE(managed_path,generation_id,asset_id)
);

CREATE TABLE session_holds (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    generation_id TEXT NOT NULL,
    FOREIGN KEY(generation_id,asset_id) REFERENCES fallback_generations(id,asset_id) ON DELETE RESTRICT
);

CREATE TABLE output_holds (
    output_id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    group_id TEXT NOT NULL,
    profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    destination_id TEXT NOT NULL,
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    variant_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT,
    FOREIGN KEY(group_id) REFERENCES groups(id) ON DELETE RESTRICT,
    FOREIGN KEY(destination_id) REFERENCES destinations(id) ON DELETE RESTRICT,
    FOREIGN KEY(variant_id,asset_id,generation_id) REFERENCES fallback_variants(id,asset_id,generation_id) ON DELETE RESTRICT
);

CREATE TABLE events (
    id TEXT PRIMARY KEY NOT NULL,
    created_at INTEGER NOT NULL,
    payload BLOB NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    output_id TEXT,
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX sessions_one_active_stream ON sessions(stream_id) WHERE state IN ('STARTING','RUNNING','STOPPING');

CREATE INDEX sessions_cleanup_stream ON sessions(stream_id,cleanup_pending);

CREATE INDEX fallback_assets_created ON fallback_assets(created_at,id);

CREATE INDEX fallback_assets_state_created ON fallback_assets(state,created_at,id);

CREATE INDEX streams_created ON streams(created_at,id);

CREATE INDEX profiles_created ON profiles(created_at,id);

CREATE INDEX groups_created ON groups(created_at,id);

CREATE INDEX destinations_created ON destinations(created_at,id);

CREATE INDEX fallback_variants_created ON fallback_variants(created_at,id);

CREATE INDEX fallback_variants_state_created ON fallback_variants(state,created_at,id);

CREATE INDEX sessions_created ON sessions(created_at,id);

CREATE INDEX sessions_state_created ON sessions(state,created_at,id);

CREATE INDEX outputs_created ON outputs(created_at,id);

CREATE INDEX command_receipts_created ON command_receipts(created_at,id);

CREATE INDEX command_receipts_state_created ON command_receipts(state,created_at,id);

CREATE INDEX cleanup_intents_created ON cleanup_intents(created_at,id);

CREATE INDEX cleanup_intents_acknowledged_created ON cleanup_intents(acknowledged,created_at,id);

CREATE INDEX events_created ON events(created_at,id);

CREATE INDEX groups_stream_id_created ON groups(stream_id,created_at,id);

CREATE INDEX destinations_stream_id_created ON destinations(stream_id,created_at,id);

CREATE INDEX fallback_variants_asset_id_created ON fallback_variants(asset_id,created_at,id);

CREATE INDEX fallback_variants_asset_id_state_created ON fallback_variants(asset_id,state,created_at,id);

CREATE INDEX sessions_stream_id_created ON sessions(stream_id,created_at,id);

CREATE INDEX sessions_stream_id_state_created ON sessions(stream_id,state,created_at,id);

CREATE INDEX outputs_session_id_created ON outputs(session_id,created_at,id);

CREATE INDEX events_session_id_created ON events(session_id,created_at,id);

CREATE INDEX command_receipts_session_id_created ON command_receipts(session_id,created_at,id);

CREATE INDEX command_receipts_session_id_state_created ON command_receipts(session_id,state,created_at,id);

CREATE INDEX cleanup_intents_stream_id_created ON cleanup_intents(stream_id,created_at,id);

CREATE INDEX cleanup_intents_stream_id_acknowledged_created ON cleanup_intents(stream_id,acknowledged,created_at,id);

CREATE INDEX cleanup_intents_session_id_acknowledged_created ON cleanup_intents(session_id,acknowledged,created_at,id);

CREATE INDEX output_holds_session_id ON output_holds(session_id);

CREATE INDEX output_holds_stream_id ON output_holds(stream_id);

CREATE INDEX output_holds_group_id ON output_holds(group_id);

CREATE INDEX output_holds_profile_id ON output_holds(profile_id);

CREATE INDEX output_holds_destination_id ON output_holds(destination_id);

CREATE INDEX output_holds_asset_id ON output_holds(asset_id);

CREATE INDEX output_holds_variant_id ON output_holds(variant_id);

CREATE INDEX session_holds_stream_id ON session_holds(stream_id);

CREATE INDEX session_holds_asset_id ON session_holds(asset_id);

CREATE INDEX streams_asset_id ON streams(asset_id);

CREATE INDEX groups_profile_id ON groups(profile_id);

CREATE INDEX fallback_generations_created ON fallback_generations(created_at,id);
CREATE INDEX fallback_generations_asset_created ON fallback_generations(asset_id,created_at,id);
CREATE INDEX fallback_generations_pending ON fallback_generations(state,issued,cleanup_pending,created_at,id);
CREATE UNIQUE INDEX fallback_generations_one_executor ON fallback_generations((1)) WHERE state IN ('CLAIMED','ISSUED') OR issued=1 OR cleanup_pending=1;
CREATE INDEX owned_paths_generation_created ON owned_paths(generation_id,created_at,id);
CREATE INDEX fallback_variants_generation_created ON fallback_variants(generation_id,created_at,id);
CREATE INDEX output_holds_generation ON output_holds(generation_id);
CREATE INDEX session_holds_generation ON session_holds(generation_id);

CREATE UNIQUE INDEX cleanup_intents_one_file ON cleanup_intents(file_id) WHERE kind='file';
CREATE INDEX cleanup_intents_artifact_generation ON cleanup_intents(artifact_generation_id,created_at,id);
