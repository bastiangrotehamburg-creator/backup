# Installationsanleitung: Acronis Monitoring & Zabbix Pipeline

Diese Anleitung beschreibt Schritt für Schritt den Aufbau der gesamten Monitoring-Pipeline (PostgreSQL-Datenbank, Go-Collector, Zabbix-Exporter, Keycloak Importer und Zabbix-Anbindung) auf einem Linux-Server.

---

## 1. Systemvoraussetzungen

- **Go (Golang)** >= 1.20
- **PostgreSQL** >= 14
- **Zabbix Server** >= 6.0 / 7.0

---

## 2. Datenbank-Setup

Erstelle einen PostgreSQL-Benutzer und die Datenbank für den Collector:

```sql
CREATE DATABASE acronis_collector;
CREATE USER acronis_collector WITH PASSWORD 'dein_sicheres_passwort';
GRANT ALL PRIVILEGES ON DATABASE acronis_collector TO acronis_collector;
```

Verbinde dich anschließend mit der Datenbank (`acronis_collector`) und lege die Tabellenstrukturen an:

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

```

---

## 3. Projektstruktur & Konfiguration

Lege das Projektverzeichnis an (z. B. `/opt/acronis-monitoring`) und platziere dort deine Go-Skripte (`acronis_collector`, `zabbix-exporter` und `keycloak_importer`).

Erstelle im Hauptverzeichnis eine zentrale `.env`-Datei:

```env
KEYCLOAK_SERVER_URL=https://k8s-keycloak.highq.org
KEYCLOAK_REALM=cloud_backups
KEYCLOAK_CLIENT_ID=acronis-collector
KEYCLOAK_CLIENT_SECRET=dein_sicheres_passwort
BASE_URL=https://backup.ionos.com
APP_DEBUG=false
ZABBIX_PORT=8080
API_TOKEN=dein_sicherer_token
API_DEBUG=true
DB_CONNECTION_STRING="host=localhost port=5432 user=acronis_collector password=dein_sicheres_passwort dbname=acronis_collector sslmode=disable"
```

---

## 4. Systemd-Dienste einrichten

### A. Zabbix-Exporter als Dienst

Erstelle die Datei `/etc/systemd/system/acronis-exporter.service`:

```ini
[Unit]
Description=Acronis Zabbix Exporter
After=network.target postgresql.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/acronis-monitoring
ExecStart=/opt/acronis-monitoring/zabbix-exporter
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

### B. Collector als Cronjob

Führe den Collector regelmäßig per Cronjob aus (z. B. alle 15 Minuten):

```bash
*/15 * * * * cd /opt/acronis-monitoring && /usr/bin/acronis_collector >> /var/log/acronis_collector.log 2>&1
```

---

## 5. Dienste aktivieren und starten

```bash
# Dienste neu laden
sudo systemctl daemon-reload

# Exporter aktivieren und starten
sudo systemctl enable --now acronis-exporter

# Status prüfen
sudo systemctl status acronis-exporter
```

---

## 6. Keycloak Client Importer (CLI)

Der `keycloak_importer` ist ein Go-basiertes CLI-Tool, mit dem Mandanten-Konfigurationen und Zugangsdaten als Client-Attribute direkt in Keycloak angelegt oder ausgelesen werden können.

### Kompilieren
```bash
go build -o keycloak_importer keycloak_importer.go
```

### Verwendung
1. **Alle vorhandenen Clients auflisten:**
   ```bash
   ./keycloak_importer -list
   ```
2. **Neuen Client interaktiv anlegen:**
   ```bash
   ./keycloak_importer
   ```
   *Hinweis:* Das Tool übernimmt die Basis-URL automatisch aus der `.env`-Datei und fragt Client-ID, Benutzernamen sowie Passwort direkt über die Befehlszeile ab.

---

## 7. Zabbix-Konfiguration

1. Importiere die `zabbix_template_acronis.yaml` in Zabbix unter **Data collection -> Templates -> Import**.
2. Erstelle einen Host für den Kunden in Zabbix und weise ihm das Template zu.
3. Passe die Host-Makros an:
   - `{$ACRONIS_URL}`: `http://<SERVER-IP>:8080/zabbix/backups`
   - `{$ACRONIS_TOKEN}`: Dein in der `.env` hinterlegter Token
   - `{$ACRONIS_CLOUD}`: Exakter Name des Mandanten (z. B. `Kunde_VBB`)