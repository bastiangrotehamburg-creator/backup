-- Update für bestehende Installationen. Kann gefahrlos mehrfach ausgeführt werden
-- (nur IF NOT EXISTS, es werden keine Daten geändert oder gelöscht).
-- Ausführen z. B. mit:  psql "<DB_CONNECTION_STRING>" -f update.sql

BEGIN;

-- Backup-Protokoll (Fehlergrund und Task-Details je Tagesreport)
ALTER TABLE acronis_daily_reports
	ADD COLUMN IF NOT EXISTS failure_reason TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS backup_details JSONB;

-- Soll/Ist-Vergleich laut Backup-Plan
ALTER TABLE acronis_weekly_reports
	ADD COLUMN IF NOT EXISTS expected_backup_days INT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS fulfilled_backup_days INT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS plan_state INT NOT NULL DEFAULT 2,
	ADD COLUMN IF NOT EXISTS schedule_info TEXT NOT NULL DEFAULT '';

-- Online-Gegenprüfung der Agents
ALTER TABLE acronis_agents
	ADD COLUMN IF NOT EXISTS online_source TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;

-- NEU: feste machine_id je Maschine, frühere Acronis-IDs in old_machine_ids (JSON)
CREATE TABLE IF NOT EXISTS acronis_machines (
	cloud_name VARCHAR(255) NOT NULL,
	machine_id VARCHAR(255) NOT NULL,
	machine_name VARCHAR(255) NOT NULL,
	current_id VARCHAR(255) NOT NULL,
	resource_type VARCHAR(255) NOT NULL DEFAULT '',
	tenant_id VARCHAR(255) NOT NULL DEFAULT '',
	old_machine_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (cloud_name, machine_id),
	UNIQUE (cloud_name, current_id)
);

COMMIT;

-- Hinweis: Die Tabelle acronis_machines wird beim ersten Lauf des neuen Collectors befüllt.
-- Dabei werden vorhandene Duplikate (gleiche Maschine, andere machine_id) zusammengeführt und
-- ihre Tages-/Wochenreports unter die feste ID verschoben. Vorher am besten ein DB-Backup machen:
--   pg_dump "<DB_CONNECTION_STRING>" -t 'acronis_*' > acronis_backup_vor_update.sql
--
-- Für die 2-Monats-Warnung ist keine Tabellenänderung nötig (Berechnung im Exporter).
