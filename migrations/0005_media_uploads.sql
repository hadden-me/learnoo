-- LRN-006 slice 3: which users may read which media assets.
--
-- Assets are content-addressed and shared: identical uploads reuse one
-- media_assets row. Each successful upload (new or deduplicated) records a
-- link for the uploader, so reading an asset requires having uploaded it (or
-- authoring a revision that references it). Reviewer and reader access come
-- with LRN-007 and LRN-008.
CREATE TABLE media_uploads (
    user_id        bigint      NOT NULL REFERENCES users (id),
    media_asset_id bigint      NOT NULL REFERENCES media_assets (id),
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (user_id, media_asset_id)
);

CREATE INDEX media_uploads_asset_idx ON media_uploads (media_asset_id);

-- Assets recorded before this table existed keep their uploader's access.
INSERT INTO media_uploads (user_id, media_asset_id, created_at)
SELECT created_by, id, created_at FROM media_assets
ON CONFLICT DO NOTHING;

-- Links are never removed or changed: read access only grows.
CREATE FUNCTION media_uploads_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'media_uploads is append-only';
END;
$$;

CREATE TRIGGER media_uploads_no_update_delete
    BEFORE UPDATE OR DELETE ON media_uploads
    FOR EACH ROW EXECUTE FUNCTION media_uploads_append_only();

CREATE TRIGGER media_uploads_no_truncate
    BEFORE TRUNCATE ON media_uploads
    FOR EACH STATEMENT EXECUTE FUNCTION media_uploads_append_only();
