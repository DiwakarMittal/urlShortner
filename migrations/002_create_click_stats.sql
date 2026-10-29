-- One row per (link, hour, referrer host, device class). Clicks are added
-- to the row by an upsert, so the table grows with the number of distinct
-- combinations, not with the number of clicks.
CREATE TABLE IF NOT EXISTS click_stats (
    code     TEXT        NOT NULL,
    bucket   TIMESTAMPTZ NOT NULL,           -- start of the hour, UTC
    referrer TEXT        NOT NULL DEFAULT '', -- host only; '' = direct/unknown
    device   TEXT        NOT NULL DEFAULT 'other',
    clicks   BIGINT      NOT NULL,
    PRIMARY KEY (code, bucket, referrer, device)
);