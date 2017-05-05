-- +migrate Up

CREATE TABLE config (
 config_id       INTEGER PRIMARY KEY AUTOINCREMENT
,config_repo_id  INTEGER
,config_hash     TEXT
,config_data     BLOB
,config_approved BOOLEAN

,UNIQUE(config_hash, config_repo_id)
);

ALTER TABLE builds ADD COLUMN build_config_id INTEGER;
UPDATE builds set build_config_id = 0;

ALTER TABLE repos ADD COLUMN repo_gated_conf BOOLEAN;
UPDATE repos SET repo_gated_conf = 0;

-- +migrate Down

DROP TABLE config;
