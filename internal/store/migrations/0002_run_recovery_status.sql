ALTER TABLE runs
    ADD COLUMN last_checkpoint jsonb NOT NULL DEFAULT 'null'::jsonb,
    ADD COLUMN conditions      jsonb NOT NULL DEFAULT '[]'::jsonb;
