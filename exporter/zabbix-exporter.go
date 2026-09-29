package main

import (
	"bufio"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
)

const ExpectedBearerToken = "DEIN_GEHEIMER_API_TOKEN_HIER"

type ZabbixItem struct {
	MachineID            string `json:"machine_id"`
	MachineName          string `json:"machine_name"`
	CloudName            string `json:"cloud_name"`
	BackupSuccessful     int    `json:"backup_successful"`
	HasErrors            int    `json:"has_errors"`
	SuccessfulBackupDays int    `json:"successful_backup_days"`
	IsOnline             int    `json:"is_online"`
	BuildVersion         string `json:"build_version"`
	ReleaseID            string `json:"release_id"`
	InstallerVersion     string `json:"installer_version"`
	HasUpdate            int    `json:"HasUpdate"`
	HasActiveAlert       int    `json:"has_active_alert"`
	FailureReason        string `json:"failure_reason"`
	// Herkunft des Online-Status: api, recheck, task_activity, offline_confirmed, offline_unconfirmed, no_agent
	OnlineSource string `json:"online_source"`
	// Soll/Ist laut Backup-Plan (letzte 7 vollständige Tage)
	ExpectedBackupDays  int    `json:"expected_backup_days"`
	FulfilledBackupDays int    `json:"fulfilled_backup_days"`
	PlanState           int    `json:"plan_state"` // 1 Zeitplan aktiv, 0 kein aktiver Plan, 2 nicht auswertbar
	ScheduleInfo        string `json:"schedule_info"`
}

// BackupFailure ist ein Eintrag im Backup-Protokoll: eine Maschine an einem Tag ohne erfolgreiches Backup.
type BackupFailure struct {
	ReportDate      string          `json:"report_date"`
	CloudName       string          `json:"cloud_name"`
	MachineID       string          `json:"machine_id"`
	MachineName     string          `json:"machine_name"`
	HasErrors       bool            `json:"has_errors"`
	FailureReason   string          `json:"failure_reason"`
	LastSuccessDate string          `json:"last_success_date"`
	Tasks           json.RawMessage `json:"tasks"`
}

type ZabbixAlert struct {
	ID           string    `json:"{#ALERT_ID}"`
	CloudName    string    `json:"{#CLOUD_NAME}"`
	Type         string    `json:"alert_type"`
	Severity     string    `json:"alert_severity"`
	ResourceName string    `json:"resource_name"`
	TenantID     string    `json:"tenant_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type ZabbixHistoryAlert struct {
	ID           string    `json:"{#HISTORY_ID}"`
	AlertID      string    `json:"alert_id"`
	CloudName    string    `json:"{#CLOUD_NAME}"`
	Type         string    `json:"alert_type"`
	Severity     string    `json:"alert_severity"`
	ResourceName string    `json:"resource_name"`
	CreatedAt    time.Time `json:"created_at"`
	ResolvedAt   time.Time `json:"resolved_at"`
}

// KPIStats beschreibt die Backup-Quote über einen Zeitraum.
// Basis sind die Tagesreports: eine Maschine an einem Tag = ein "Backup-Tag".
type KPIStats struct {
	BackupDaysTotal  int     `json:"backup_days_total"`
	BackupDaysFailed int     `json:"backup_days_failed"`
	FailedPercent    float64 `json:"failed_percent"`
	SuccessPercent   float64 `json:"success_percent"`
	DaysWithErrors   int     `json:"days_with_errors"`
	ErrorsPercent    float64 `json:"errors_percent"`
	DatesWithData    int     `json:"dates_with_data"`
	FirstDate        string  `json:"first_date"`
	LastDate         string  `json:"last_date"`
}

type KPICloud struct {
	CloudName string `json:"cloud_name"`
	KPIStats
}

type KPIMachine struct {
	CloudName   string `json:"cloud_name"`
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name"`
	KPIStats
}

type KPIDay struct {
	Date             string  `json:"date"`
	BackupDaysTotal  int     `json:"backup_days_total"`
	BackupDaysFailed int     `json:"backup_days_failed"`
	FailedPercent    float64 `json:"failed_percent"`
}

type KPIResponse struct {
	PeriodDays      int          `json:"period_days"`
	RequestedFrom   string       `json:"requested_from"`
	From            string       `json:"from"`
	To              string       `json:"to"`
	CloudFilter     string       `json:"cloud_filter"`
	AvailableClouds []string     `json:"available_clouds"`
	Total           KPIStats     `json:"total"`
	Clouds          []KPICloud   `json:"clouds"`
	Machines        []KPIMachine `json:"machines"`
	Daily           []KPIDay     `json:"daily"`
}

//go:embed kpi.html
var kpiPage []byte

func percent(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(part)*10000/float64(total)) / 100
}

func (k *KPIStats) calculate() {
	k.FailedPercent = percent(k.BackupDaysFailed, k.BackupDaysTotal)
	k.ErrorsPercent = percent(k.DaysWithErrors, k.BackupDaysTotal)
	if k.BackupDaysTotal > 0 {
		k.SuccessPercent = math.Round((100-k.FailedPercent)*100) / 100
	}
}

// kpiRange ist der Auswertungszeitraum der KPI.
type kpiRange struct {
	Days          int
	RequestedFrom time.Time
	From          time.Time
	To            time.Time
}

func parseDay(v string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", strings.TrimSpace(v), time.Local)
}

func spanDays(from, to time.Time) int {
	return int(math.Round(to.Sub(from).Hours()/24)) + 1
}

// kpiPeriod ermittelt den Zeitraum:
//   - ?days=N: die letzten N vollständigen Tage bis gestern (Standard 365)
//   - ?from=YYYY-MM-DD&to=YYYY-MM-DD: fester Zeitraum, z. B. ein Monat oder Kalenderjahr (to optional)
//
// Das Enddatum wird auf gestern begrenzt. KPI_START_DATE verschiebt den Beginn nach hinten,
// damit fehlerhafte Altdaten nicht einfließen.
func kpiPeriod(r *http.Request) (kpiRange, error) {
	var p kpiRange
	q := r.URL.Query()
	now := time.Now()
	yesterday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -1)

	p.To = yesterday
	if v := q.Get("to"); v != "" {
		to, err := parseDay(v)
		if err != nil {
			return p, fmt.Errorf("to muss im Format YYYY-MM-DD sein")
		}
		if to.Before(yesterday) {
			p.To = to
		}
	}

	if v := q.Get("from"); v != "" {
		from, err := parseDay(v)
		if err != nil {
			return p, fmt.Errorf("from muss im Format YYYY-MM-DD sein")
		}
		p.RequestedFrom = from
	} else {
		days := 365
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 3650 {
				return p, fmt.Errorf("days muss eine Zahl zwischen 1 und 3650 sein")
			}
			days = n
		}
		p.RequestedFrom = p.To.AddDate(0, 0, -(days - 1))
	}

	if p.RequestedFrom.After(p.To) {
		return p, fmt.Errorf("Der Zeitraum beginnt nach seinem Ende (from %s, to %s)",
			p.RequestedFrom.Format("2006-01-02"), p.To.Format("2006-01-02"))
	}
	p.Days = spanDays(p.RequestedFrom, p.To)
	if p.Days > 3660 {
		return p, fmt.Errorf("Der Zeitraum darf höchstens 10 Jahre umfassen")
	}

	p.From = p.RequestedFrom
	if v := os.Getenv("KPI_START_DATE"); v != "" {
		start, err := parseDay(v)
		if err != nil {
			return p, fmt.Errorf("KPI_START_DATE muss im Format YYYY-MM-DD sein")
		}
		if start.After(p.From) {
			p.From = start
		}
	}
	return p, nil
}

// Die Protokoll-Spalten legt der Collector an. Bis dahin liefert der Exporter
// /zabbix/backups ohne Fehlergrund aus, statt komplett mit einem SQL-Fehler auszufallen.
var protocolColumnsReady atomic.Bool

const protocolMigrationSQL = `ALTER TABLE acronis_daily_reports
	ADD COLUMN IF NOT EXISTS failure_reason TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS backup_details JSONB;`

// Spalten für den Soll/Ist-Vergleich legt der Collector an.
var weeklyPlanColumnsReady atomic.Bool

func checkWeeklyPlanColumns(db *sql.DB) bool {
	if weeklyPlanColumnsReady.Load() {
		return true
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM pg_attribute
		WHERE attrelid = to_regclass('acronis_weekly_reports')
		  AND attname IN ('expected_backup_days', 'fulfilled_backup_days', 'plan_state', 'schedule_info')
		  AND NOT attisdropped
	`).Scan(&n)
	if err == nil && n == 4 {
		weeklyPlanColumnsReady.Store(true)
		return true
	}
	return false
}

// Spalten für die Online-Gegenprüfung legt ebenfalls der Collector an.
var agentColumnsReady atomic.Bool

func checkAgentColumns(db *sql.DB) bool {
	if agentColumnsReady.Load() {
		return true
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM pg_attribute
		WHERE attrelid = to_regclass('acronis_agents')
		  AND attname IN ('online_source', 'last_seen_at')
		  AND NOT attisdropped
	`).Scan(&n)
	if err == nil && n == 2 {
		agentColumnsReady.Store(true)
		return true
	}
	return false
}

func checkProtocolColumns(db *sql.DB) bool {
	if protocolColumnsReady.Load() {
		return true
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM pg_attribute
		WHERE attrelid = to_regclass('acronis_daily_reports')
		  AND attname IN ('failure_reason', 'backup_details')
		  AND NOT attisdropped
	`).Scan(&n)
	if err == nil && n == 2 {
		protocolColumnsReady.Store(true)
		fmt.Println("Backup-Protokoll: Spalten gefunden, Fehlergründe werden ausgeliefert.")
		return true
	}
	return false
}

func loadEnvFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
}

func authenticateRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Wenn API_DEBUG auf true steht, Authentifizierung überspringen
		if strings.ToLower(os.Getenv("API_DEBUG")) == "true" {
			next(w, r)
			return
		}

		expectedToken := os.Getenv("ACRONIS_EXPORTER_TOKEN")
		if expectedToken == "" {
			next(w, r)
			return
		}

		// Browser (KPI-Seite) melden sich per Basic Auth mit DASHBOARD_USER/DASHBOARD_PASSWORD an
		dashUser := os.Getenv("DASHBOARD_USER")
		dashPass := os.Getenv("DASHBOARD_PASSWORD")
		if user, pass, ok := r.BasicAuth(); ok {
			if dashUser != "" && dashPass != "" &&
				subtle.ConstantTimeCompare([]byte(user), []byte(dashUser)) == 1 &&
				subtle.ConstantTimeCompare([]byte(pass), []byte(dashPass)) == 1 {
				next(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="Acronis Backup-KPI", charset="UTF-8"`)
			http.Error(w, "Unauthorized: Benutzername oder Passwort falsch", http.StatusUnauthorized)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			if dashUser != "" && dashPass != "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Acronis Backup-KPI", charset="UTF-8"`)
			}
			http.Error(w, "Unauthorized: No Authorization header provided", http.StatusUnauthorized)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			http.Error(w, "Unauthorized: Invalid Authorization format", http.StatusUnauthorized)
			return
		}

		if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expectedToken)) != 1 {
			http.Error(w, "Forbidden: Invalid API Token", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

func main() {
	loadEnvFile(".env")

	connStr := os.Getenv("DB_CONNECTION_STRING")
	if connStr == "" {
		panic("DB_CONNECTION_STRING ist nicht gesetzt")
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		panic(fmt.Sprintf("DB Verbindungsfehler: %v", err))
	}
	defer db.Close()

	if !checkProtocolColumns(db) {
		fmt.Printf("WARNUNG: Spalten für das Backup-Protokoll fehlen in acronis_daily_reports. "+
			"/zabbix/backups läuft ohne Fehlergrund, /zabbix/backups/failures ist deaktiviert, "+
			"bis der neue Collector gelaufen ist oder folgendes SQL ausgeführt wurde:\n%s\n", protocolMigrationSQL)
	}

	http.HandleFunc("/zabbix/backups", authenticateRequest(func(w http.ResponseWriter, r *http.Request) {
		query := `
			SELECT DISTINCT ON (d.machine_id) 
				d.cloud_name, 
				d.machine_id, 
				d.machine_name, 
				d.backup_successful, 
				d.has_errors,
				COALESCE(w.successful_backup_days, 0) as successful_backup_days,
				CASE WHEN a.is_online IS NULL THEN 2 WHEN a.is_online THEN 1 ELSE 0 END as is_online,
				COALESCE(a.build_version, 'Unbekannt') as build_version,
				COALESCE(a.release_id, 'Unbekannt') as release_id,
				COALESCE(a.installer_version, 'Unbekannt') as installer_version,
				CASE WHEN al.id IS NOT NULL THEN 1 ELSE 0 END as has_active_alert,
				{{FAILURE_REASON}} as failure_reason,
				CASE WHEN a.is_online IS NULL THEN 'no_agent' ELSE {{ONLINE_SOURCE}} END as online_source,
				{{WEEKLY_PLAN}}
			FROM acronis_daily_reports d
			LEFT JOIN acronis_weekly_reports w 
				ON d.machine_id = w.machine_id AND d.cloud_name = w.cloud_name
			-- Genau ein Agent pro Maschine: zuerst Treffer über die ID, dann über den Hostnamen.
			-- Veraltete Einträge (im letzten Collector-Lauf nicht mehr gesehen) werden ignoriert,
			-- bei mehreren aktuellen Agents mit gleichem Hostnamen gewinnt der Online-Agent.
			LEFT JOIN LATERAL (
				SELECT ag.* FROM acronis_agents ag
				WHERE ag.cloud_name = d.cloud_name
				  AND (LOWER(ag.id) = LOWER(d.machine_id)
				       OR SPLIT_PART(LOWER(ag.hostname), '.', 1) = SPLIT_PART(LOWER(d.machine_name), '.', 1))
				  {{AGENT_FRESH}}
				ORDER BY (LOWER(ag.id) = LOWER(d.machine_id)) DESC {{AGENT_ORDER}}, ag.is_online DESC
				LIMIT 1
			) a ON true
			LEFT JOIN LATERAL (
				SELECT id FROM acronis_alerts 
				WHERE cloud_name = d.cloud_name 
				  AND LOWER(resource_name) = LOWER(d.machine_name)
				  AND created_at >= NOW() - INTERVAL '24 hours'
				LIMIT 1
			) al ON true
			WHERE d.machine_id IS NOT NULL AND d.machine_id != ''
			ORDER BY d.machine_id, d.report_date DESC
		`

		if checkProtocolColumns(db) {
			query = strings.Replace(query, "{{FAILURE_REASON}}", "COALESCE(d.failure_reason, '')", 1)
		} else {
			query = strings.Replace(query, "{{FAILURE_REASON}}", "''", 1)
		}
		if checkWeeklyPlanColumns(db) {
			query = strings.Replace(query, "{{WEEKLY_PLAN}}", `COALESCE(w.expected_backup_days, 0),
				COALESCE(w.fulfilled_backup_days, 0),
				COALESCE(w.plan_state, 2),
				COALESCE(w.schedule_info, '')`, 1)
		} else {
			query = strings.Replace(query, "{{WEEKLY_PLAN}}", "0, 0, 2, ''", 1)
		}
		if checkAgentColumns(db) {
			// Nur Agents aus dem letzten Collector-Lauf der Cloud; solange noch kein Lauf last_seen_at
			// gesetzt hat (direkt nach der Migration), werden alle Agents berücksichtigt.
			query = strings.Replace(query, "{{AGENT_FRESH}}", `AND COALESCE(
					ag.last_seen_at >= (SELECT MAX(x.last_seen_at) FROM acronis_agents x WHERE x.cloud_name = ag.cloud_name) - INTERVAL '1 hour',
					(SELECT MAX(x.last_seen_at) FROM acronis_agents x WHERE x.cloud_name = ag.cloud_name) IS NULL
				)`, 1)
			query = strings.Replace(query, "{{AGENT_ORDER}}", ", ag.last_seen_at DESC", 1)
			query = strings.Replace(query, "{{ONLINE_SOURCE}}", "COALESCE(NULLIF(a.online_source, ''), 'api')", 1)
		} else {
			query = strings.Replace(query, "{{AGENT_FRESH}}", "", 1)
			query = strings.Replace(query, "{{AGENT_ORDER}}", "", 1)
			query = strings.Replace(query, "{{ONLINE_SOURCE}}", "'api'", 1)
		}

		rows, err := db.Query(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var items []ZabbixItem
		for rows.Next() {
			var item ZabbixItem
			var success, hasErrors bool

			if err := rows.Scan(
				&item.CloudName,
				&item.MachineID,
				&item.MachineName,
				&success,
				&hasErrors,
				&item.SuccessfulBackupDays,
				&item.IsOnline,
				&item.BuildVersion,
				&item.ReleaseID,
				&item.InstallerVersion,
				&item.HasActiveAlert,
				&item.FailureReason,
				&item.OnlineSource,
				&item.ExpectedBackupDays,
				&item.FulfilledBackupDays,
				&item.PlanState,
				&item.ScheduleInfo,
			); err != nil {
				continue
			}

			if success {
				item.BackupSuccessful = 1
			}
			if hasErrors {
				item.HasErrors = 1
			}

			if item.ReleaseID != item.InstallerVersion {
				item.HasUpdate = 1
			} else {
				item.HasUpdate = 0
			}

			items = append(items, item)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	}))

	http.HandleFunc("/zabbix/alerts", authenticateRequest(func(w http.ResponseWriter, r *http.Request) {
		query := `
			SELECT id, cloud_name, type, severity, COALESCE(resource_name, ''), COALESCE(tenant_id, ''), created_at 
			FROM acronis_alerts 
			ORDER BY created_at DESC
		`

		rows, err := db.Query(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var alerts []ZabbixAlert
		for rows.Next() {
			var alert ZabbixAlert
			if err := rows.Scan(&alert.ID, &alert.CloudName, &alert.Type, &alert.Severity, &alert.ResourceName, &alert.TenantID, &alert.CreatedAt); err != nil {
				continue
			}
			alerts = append(alerts, alert)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(alerts)
	}))

	http.HandleFunc("/zabbix/alerts/history", authenticateRequest(func(w http.ResponseWriter, r *http.Request) {
		query := `
		SELECT id, alert_id, cloud_name, type, severity, COALESCE(resource_name, ''), created_at, resolved_at 
		FROM acronis_alert_history 
		ORDER BY resolved_at DESC 
		LIMIT 100
	`

		rows, err := db.Query(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var history []ZabbixHistoryAlert
		for rows.Next() {
			var item ZabbixHistoryAlert
			if err := rows.Scan(&item.ID, &item.AlertID, &item.CloudName, &item.Type, &item.Severity, &item.ResourceName, &item.CreatedAt, &item.ResolvedAt); err != nil {
				continue
			}
			history = append(history, item)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(history)
	}))
	http.HandleFunc("/zabbix/backups/kpi", authenticateRequest(func(w http.ResponseWriter, r *http.Request) {
		period, err := kpiPeriod(r)
		from, to := period.From, period.To
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Eine Abfrage für alle drei Ebenen: pro Maschine, pro Cloud und gesamt
		query := `
			SELECT
				GROUPING(cloud_name) AS g_cloud,
				GROUPING(machine_id) AS g_machine,
				COALESCE(cloud_name, ''),
				COALESCE(machine_id, ''),
				COALESCE(MAX(machine_name), ''),
				COUNT(*),
				COUNT(*) FILTER (WHERE NOT COALESCE(backup_successful, false)),
				COUNT(*) FILTER (WHERE COALESCE(has_errors, false)),
				COUNT(DISTINCT report_date::date),
				COALESCE(MIN(report_date::date)::text, ''),
				COALESCE(MAX(report_date::date)::text, '')
			FROM acronis_daily_reports
			WHERE machine_id IS NOT NULL AND machine_id != ''
			  AND report_date::date >= $1::date
			  AND report_date::date <= $2::date
			  AND ($3 = '' OR cloud_name = $3)
			GROUP BY GROUPING SETS ((cloud_name, machine_id), (cloud_name), ())
			ORDER BY g_cloud, g_machine, cloud_name, machine_id
		`

		cloudFilter := r.URL.Query().Get("cloud")
		fromStr, toStr := from.Format("2006-01-02"), to.Format("2006-01-02")

		rows, err := db.Query(query, fromStr, toStr, cloudFilter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		resp := KPIResponse{
			PeriodDays:      period.Days,
			RequestedFrom:   period.RequestedFrom.Format("2006-01-02"),
			From:            fromStr,
			To:              toStr,
			CloudFilter:     cloudFilter,
			AvailableClouds: []string{},
			Clouds:          []KPICloud{},
			Machines:        []KPIMachine{},
			Daily:           []KPIDay{},
		}

		for rows.Next() {
			var gCloud, gMachine int
			var cloudName, machineID, machineName string
			var stats KPIStats

			if err := rows.Scan(
				&gCloud, &gMachine,
				&cloudName, &machineID, &machineName,
				&stats.BackupDaysTotal,
				&stats.BackupDaysFailed,
				&stats.DaysWithErrors,
				&stats.DatesWithData,
				&stats.FirstDate,
				&stats.LastDate,
			); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			stats.calculate()

			switch {
			case gCloud == 1:
				resp.Total = stats
			case gMachine == 1:
				resp.Clouds = append(resp.Clouds, KPICloud{CloudName: cloudName, KPIStats: stats})
			default:
				resp.Machines = append(resp.Machines, KPIMachine{
					CloudName:   cloudName,
					MachineID:   machineID,
					MachineName: machineName,
					KPIStats:    stats,
				})
			}
		}
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Tagesverlauf für die Zeitleiste
		dailyRows, err := db.Query(`
			SELECT report_date::date::text,
				COUNT(*),
				COUNT(*) FILTER (WHERE NOT COALESCE(backup_successful, false))
			FROM acronis_daily_reports
			WHERE machine_id IS NOT NULL AND machine_id != ''
			  AND report_date::date >= $1::date
			  AND report_date::date <= $2::date
			  AND ($3 = '' OR cloud_name = $3)
			GROUP BY report_date::date
			ORDER BY report_date::date
		`, fromStr, toStr, cloudFilter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer dailyRows.Close()
		for dailyRows.Next() {
			var d KPIDay
			if err := dailyRows.Scan(&d.Date, &d.BackupDaysTotal, &d.BackupDaysFailed); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			d.FailedPercent = percent(d.BackupDaysFailed, d.BackupDaysTotal)
			resp.Daily = append(resp.Daily, d)
		}

		// Alle Clouds im Zeitraum, unabhängig vom Filter (für die Auswahlliste)
		cloudRows, err := db.Query(`
			SELECT DISTINCT cloud_name FROM acronis_daily_reports
			WHERE cloud_name IS NOT NULL AND cloud_name != ''
			  AND report_date::date >= $1::date AND report_date::date <= $2::date
			ORDER BY cloud_name
		`, fromStr, toStr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer cloudRows.Close()
		for cloudRows.Next() {
			var c string
			if err := cloudRows.Scan(&c); err == nil {
				resp.AvailableClouds = append(resp.AvailableClouds, c)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))

	// KPI-Webseite. Datenabruf erfolgt aus dem Browser über dieselbe Anmeldung.
	http.HandleFunc("/kpi", authenticateRequest(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		w.Write(kpiPage)
	}))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "kpi", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	// Backup-Protokoll: alle Maschinen-Tage ohne erfolgreiches Backup inkl. Grund und Task-Details.
	// Parameter: days (Standard 30) oder from/to (YYYY-MM-DD), cloud, machine (Name-Teilstring oder ID), limit (Standard 500), pretty=1
	http.HandleFunc("/zabbix/backups/failures", authenticateRequest(func(w http.ResponseWriter, r *http.Request) {
		if !checkProtocolColumns(db) {
			http.Error(w, "Backup-Protokoll noch nicht verfügbar: Spalten fehlen in acronis_daily_reports. "+
				"Neuen Collector einmal laufen lassen oder ausführen:\n"+protocolMigrationSQL, http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()

		days := 30
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 3650 {
				http.Error(w, "days muss eine Zahl zwischen 1 und 3650 sein", http.StatusBadRequest)
				return
			}
			days = n
		}
		limit := 500
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 10000 {
				http.Error(w, "limit muss eine Zahl zwischen 1 und 10000 sein", http.StatusBadRequest)
				return
			}
			limit = n
		}

		now := time.Now()
		from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -days)
		if v := q.Get("from"); v != "" {
			f, err := parseDay(v)
			if err != nil {
				http.Error(w, "from muss im Format YYYY-MM-DD sein", http.StatusBadRequest)
				return
			}
			from = f
		}
		toStr := "9999-12-31"
		if v := q.Get("to"); v != "" {
			t, err := parseDay(v)
			if err != nil {
				http.Error(w, "to muss im Format YYYY-MM-DD sein", http.StatusBadRequest)
				return
			}
			toStr = t.Format("2006-01-02")
		}

		query := `
			SELECT
				d.report_date::date::text,
				d.cloud_name,
				d.machine_id,
				COALESCE(d.machine_name, ''),
				COALESCE(d.has_errors, false),
				CASE
					WHEN COALESCE(d.failure_reason, '') <> '' THEN d.failure_reason
					ELSE 'Kein Protokoll vorhanden (Report vor Einführung der Fehlerdiagnose)'
				END,
				COALESCE((
					SELECT MAX(s.report_date::date)::text
					FROM acronis_daily_reports s
					WHERE s.cloud_name = d.cloud_name
					  AND s.machine_id = d.machine_id
					  AND s.backup_successful
					  AND s.report_date::date < d.report_date::date
				), ''),
				COALESCE(d.backup_details, '[]'::jsonb)::text
			FROM acronis_daily_reports d
			WHERE NOT COALESCE(d.backup_successful, false)
			  AND d.machine_id IS NOT NULL AND d.machine_id != ''
			  AND d.report_date::date >= $1::date
			  AND d.report_date::date <= $5::date
			  AND ($2 = '' OR d.cloud_name = $2)
			  AND ($3 = '' OR d.machine_id = $3 OR d.machine_name ILIKE '%' || $3 || '%')
			ORDER BY d.report_date DESC, d.cloud_name, d.machine_name
			LIMIT $4
		`

		rows, err := db.Query(query, from.Format("2006-01-02"), q.Get("cloud"), q.Get("machine"), limit, toStr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		failures := []BackupFailure{}
		for rows.Next() {
			var f BackupFailure
			var tasks string
			if err := rows.Scan(&f.ReportDate, &f.CloudName, &f.MachineID, &f.MachineName, &f.HasErrors, &f.FailureReason, &f.LastSuccessDate, &tasks); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f.Tasks = json.RawMessage(tasks)
			failures = append(failures, f)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if q.Get("pretty") != "" {
			enc.SetIndent("", "  ")
		}
		enc.Encode(failures)
	}))

	PORT := ":" + os.Getenv("ZABBIX_PORT")
	fmt.Println("Zabbix Exporter läuft auf Port " + os.Getenv("ZABBIX_PORT") + "...")
	fmt.Println("/zabbix/alerts/history")
	fmt.Println("/zabbix/alerts")
	fmt.Println("/zabbix/backups")
	fmt.Println("/zabbix/backups/kpi?days=365&cloud=  oder  ?from=2026-01-01&to=2026-12-31")
	fmt.Println("/kpi (Webseite)")
	if os.Getenv("ACRONIS_EXPORTER_TOKEN") != "" && (os.Getenv("DASHBOARD_USER") == "" || os.Getenv("DASHBOARD_PASSWORD") == "") {
		fmt.Println("HINWEIS: Für die KPI-Webseite DASHBOARD_USER und DASHBOARD_PASSWORD setzen, sonst ist keine Anmeldung im Browser möglich.")
	}
	fmt.Println("/zabbix/backups/failures?days=30&machine=&cloud=&pretty=1")
	http.ListenAndServe(PORT, nil)
}
