# Acronis Cloud Monitoring & Zabbix Integration

Eine robuste, modulare Monitoring-Pipeline in Go, die Acronis Cloud-Umgebungen aggregiert, Rohdaten und Alarm-Historien in PostgreSQL speichert und einheitliche JSON-Endpunkte für die Zabbix-Integration bereitstellt.

## Funktionen

- **Automatisierte Datenerfassung**: Synchronisiert Maschinenressourcen, Agenten, Tasks, Aktivitäten und aktive Alarme von den Acronis Cloud-APIs.
- **Rohdaten-Audit-Logging**: Speichert rohe API-Antworten (`acronis_raw_logs`) für vollständige Nachvollziehbarkeit und Audits.
- **Alarm-Lebenszyklus-Tracking**: Gleicht aktive Alarme automatisch ab und archiviert behobene Probleme in einer Historientabelle (`acronis_alert_history`).
- **Zabbix-Exporter**: Stellt sichere HTTP-Endpunkte (`/zabbix/backups`, `/zabbix/alerts`) bereit, die Backup-Status, wöchentliche Erfolgsmetriken, Agenten-Gesundheit und aktive Alarm-Flags (`has_active_alert`) kombinieren.
- **Flexible Sicherheit**: Unterstützt über `.env`-Dateien geladene Bearer-Token-Authentifizierung mit einem optionalen `API_DEBUG`-Schalter für lokale Fehlersuche.
- **Mandantenfähigkeit**: Entwickelt für Multi-Cloud-Umgebungen durch dynamische Filterung und Host-basierte Zabbix-Makros.

---

## Architektur

```
[Acronis Cloud API] 
       │
       ▼ (Polling via Collector)
[acronis_collector.go] ──► [PostgreSQL Database]
                                  │
                                  ▼ (Queried via Exporter)
                          [zabbix-exporter.go] 
                                  │
                                  ▼ (HTTP + Bearer Token)
                           [Zabbix Server / Agent]
```

---

## Projektstruktur

- `acronis_collector.go`: Kern-Datensynchronisations-Engine (verwaltet Paginierung, Statusabgleich, Rohdaten-Logging und Alarm-Archivierung).
- `zabbix-exporter.go`: HTTP-Server für schlanke JSON-Feeds zur Zabbix Low-Level Discovery (LLD).
- `zabbix_template_acronis.yaml`: Ein importbereites Zabbix-Template mit makrogesteuerten URLs, Token-Headern und mandantenspezifischer Filterung.

---

## Erste Schritte

### 1. Datenbank-Setup

Führe das SQL-Skript zur Tabellenerstellung in deiner PostgreSQL-Instanz aus:

```sql
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
	"online_source" TEXT NOT NULL DEFAULT '',
	"last_seen_at" TIMESTAMPTZ NULL DEFAULT NULL,
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
	"failure_reason" TEXT NOT NULL DEFAULT '',
	"backup_details" JSONB NULL DEFAULT NULL,
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
	"expected_backup_days" INTEGER NOT NULL DEFAULT 0,
	"fulfilled_backup_days" INTEGER NOT NULL DEFAULT 0,
	"plan_state" INTEGER NOT NULL DEFAULT 2,
	"schedule_info" TEXT NOT NULL DEFAULT '',
	PRIMARY KEY ("id"),
	UNIQUE ("cloud_name", "machine_id")
);

```

### 2. Konfiguration (`.env`)

Erstelle eine `.env`-Datei im Stammverzeichnis:

```env
KEYCLOAK_SERVER_URL=https://hq-keycloak.highq.org
KEYCLOAK_REALM=cloud_backups
KEYCLOAK_CLIENT_ID=acronis-collector
KEYCLOAK_CLIENT_SECRET=sicher
KEYCLOAK_BASE_URL=https://backup.ionos.com
APP_DEBUG=false
ZABBIX_PORT=8090
ACRONIS_EXPORTER_TOKEN=sicher
API_DEBUG=false
DB_CONNECTION_STRING="host=10.1.60.224 port=5432 user=acronis_collector password=sicher dbname=acronis_collector sslmode=disable"
KPI_START_DATE=2026-09-10
DASHBOARD_USER=highq
DASHBOARD_PASSWORD=sicher
AGENT_RECHECK_SECONDS=20    # 0 = keine zweite Abfrage
AGENT_ACTIVITY_MINUTES=30   # 0 = Task-Aktivität nicht berücksichtigen
```

### 3. Exporter starten

Kompilieren und Ausführen des Exporters:

```bash
go build -o zabbix-exporter zabbix-exporter.go
./zabbix-exporter
```

---

## Zabbix-Integration

1. Importiere `zabbix_template_acronis.yaml` in deinen Zabbix-Server (**Data collection -> Templates -> Import**).
2. Weise das Template **Acronis Backup Monitoring** dem jeweiligen Host zu.
3. Konfiguriere die Host-Makros:
   - `{$ACRONIS_URL}`: Endpunkt-URL (z. B. `http://<exporter-ip>:8080/zabbix/backups`)
   - `{$ACRONIS_TOKEN}`: Der in der `.env` hinterlegte Bearer-Token
   - `{$ACRONIS_CLOUD}`: Mandanten-/Cloud-Name als Filter passend zur Datenquelle

---

## Lizenz

Dieses Projekt ist Open-Source und für die interne IT-Infrastrukturautomatisierung und das Monitoring ausgelegt.