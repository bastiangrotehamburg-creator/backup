CREATE TABLE IF NOT EXISTS "acronis_activities" (
	"id" BIGINT NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"type" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"state" VARCHAR(50) NULL DEFAULT NULL::character varying,
	"task_id" BIGINT NULL DEFAULT NULL,
	"tenant_id" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"context" JSONB NULL DEFAULT NULL,
	"created_at" TIMESTAMP NULL DEFAULT NULL,
	PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "acronis_agents" (
	"id" VARCHAR(255) NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"hostname" VARCHAR(255) NOT NULL,
	"is_online" BOOLEAN NULL DEFAULT NULL,
	"build_version" VARCHAR(50) NULL DEFAULT NULL::character varying,
	"release_id" VARCHAR(50) NULL DEFAULT NULL,
	"installer_version" VARCHAR(50) NULL DEFAULT NULL,
	PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "acronis_alert_history" (
	"id" SERIAL NOT NULL,
	"alert_id" VARCHAR(255) NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"type" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"severity" VARCHAR(50) NULL DEFAULT NULL::character varying,
	"resource_name" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"tenant_id" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"created_at" TIMESTAMP NULL DEFAULT NULL,
	"resolved_at" TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP,
	"details" TEXT NULL DEFAULT NULL,
	PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "acronis_alerts" (
	"id" VARCHAR(255) NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"type" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"severity" VARCHAR(50) NULL DEFAULT NULL::character varying,
	"created_at" TIMESTAMP NULL DEFAULT NULL,
	"resource_name" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"tenant_id" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"details" TEXT NULL DEFAULT NULL,
	PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "acronis_daily_reports" (
	"id" SERIAL NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"machine_id" VARCHAR(255) NOT NULL,
	"machine_name" VARCHAR(255) NOT NULL,
	"backup_successful" BOOLEAN NOT NULL,
	"has_errors" BOOLEAN NOT NULL,
	"report_date" DATE NOT NULL,
	"updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY ("id"),
	UNIQUE ("cloud_name", "machine_id", "report_date")
);

CREATE TABLE IF NOT EXISTS "acronis_raw_logs" (
	"id" SERIAL NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"endpoint" VARCHAR(255) NOT NULL,
	"raw_response" TEXT NOT NULL,
	"fetched_at" TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "acronis_tasks" (
	"id" BIGINT NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"resource_id" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"machine_name" VARCHAR(255) NULL DEFAULT NULL::character varying,
	"state" VARCHAR(50) NULL DEFAULT NULL::character varying,
	"result" JSONB NULL DEFAULT NULL,
	"error_details" JSONB NULL DEFAULT NULL,
	"created_at" TIMESTAMP NULL DEFAULT NULL,
	PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "acronis_weekly_reports" (
	"id" SERIAL NOT NULL,
	"cloud_name" VARCHAR(255) NOT NULL,
	"machine_id" VARCHAR(255) NOT NULL,
	"machine_name" VARCHAR(255) NOT NULL,
	"successful_backup_days" INTEGER NOT NULL,
	"has_errors" BOOLEAN NOT NULL,
	"report_date" TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP,
	"updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY ("id"),
	UNIQUE ("cloud_name", "machine_id")
);

CREATE TABLE IF NOT EXISTS "acronis_machines" (
	"cloud_name" VARCHAR(255) NOT NULL,
	"machine_id" VARCHAR(255) NOT NULL,
	"machine_name" VARCHAR(255) NOT NULL,
	"current_id" VARCHAR(255) NOT NULL,
	"resource_type" VARCHAR(255) NOT NULL DEFAULT '',
	"tenant_id" VARCHAR(255) NOT NULL DEFAULT '',
	"old_machine_ids" JSONB NOT NULL DEFAULT '[]'::jsonb,
	"created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
	"updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY ("cloud_name", "machine_id"),
	UNIQUE ("cloud_name", "current_id")
);
