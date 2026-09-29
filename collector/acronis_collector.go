package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // Zeitzonen-Daten einbetten, damit TZ=Europe/Berlin auch in minimalen Containern greift

	_ "github.com/lib/pq"
)

type AcronisClient struct {
	baseURL    string
	username   string
	password   string
	token      string
	httpClient *http.Client
}

type CloudConfig struct {
	Name     string
	BaseURL  string
	Username string
	Password string
}

type ResourceItem struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	CreatedAt apiTime `json:"created_at"`
}

type ResourceList struct {
	Items  []ResourceItem `json:"items"`
	Paging struct {
		Cursors struct {
			After string `json:"after"`
		} `json:"cursors"`
	} `json:"paging"`
}

// fetchResources holt Ressourcen aus dem Resource Management (optional nach Typ gefiltert)
// und folgt dem Paging-Cursor, falls die API mehrere Seiten liefert.
func (c *AcronisClient) fetchResources(typeFilter string) ([]ResourceItem, [][]byte, error) {
	var items []ResourceItem
	var pages [][]byte
	cursor := ""

	for page := 0; page < 100; page++ {
		q := url.Values{}
		if typeFilter != "" {
			q.Add("type", typeFilter)
		}
		if cursor != "" {
			q.Add("after", cursor)
		}

		endpoint := "/api/resource_management/v4/resources"
		if encoded := q.Encode(); encoded != "" {
			endpoint += "?" + encoded
		}

		data, err := c.doRequest("GET", endpoint)
		if err != nil {
			return items, pages, err
		}
		pages = append(pages, data)

		var list ResourceList
		if err := json.Unmarshal(data, &list); err != nil {
			return items, pages, fmt.Errorf("Ressourcen konnten nicht geparst werden: %w", err)
		}
		items = append(items, list.Items...)

		if list.Paging.Cursors.After == "" || len(list.Items) == 0 || list.Paging.Cursors.After == cursor {
			break
		}
		cursor = list.Paging.Cursors.After
	}

	return items, pages, nil
}

func resourceTypeSummary(items []ResourceItem) string {
	counts := make(map[string]int)
	for _, it := range items {
		t := it.Type
		if t == "" {
			t = "(ohne Typ)"
		}
		counts[t]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// apiTime akzeptiert RFC3339-Zeitstempel und toleriert null bzw. "" –
// sonst würde ein einzelner Task mit leerem Datum das Parsen der ganzen Seite abbrechen.
type apiTime struct {
	time.Time
}

func (t *apiTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Unbekanntes Format nicht als Fehler behandeln, nur als "kein Datum"
		t.Time = time.Time{}
		return nil
	}
	t.Time = parsed
	return nil
}

// Die Task Manager API v2 liefert camelCase-Felder (enqueuedAt, startedAt, completedAt).
// Mit den früheren snake_case-Tags (created_at, started_at, ...) blieben alle Zeitstempel leer.
type Task struct {
	ID          int64   `json:"id"`
	Type        string  `json:"type"`
	State       string  `json:"state"`
	EnqueuedAt  apiTime `json:"enqueuedAt"`
	StartedAt   apiTime `json:"startedAt"`
	CompletedAt apiTime `json:"completedAt"`
	UpdatedAt   apiTime `json:"updatedAt"`
	// Ergebnis steckt in result.code: ok, warning, error, cancelled, abandoned, timedout
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Resource *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"resource"`
	Policy *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"policy"`
	Context json.RawMessage `json:"context"`
	// Fallback für ältere/abweichende Antworten
	ResourceID string `json:"resource_id"`
}

type taskResult struct {
	Code  string          `json:"code"`
	Error json.RawMessage `json:"error"`
}

func (t Task) resultCode() string {
	if len(t.Result) == 0 {
		return ""
	}
	var r taskResult
	if err := json.Unmarshal(t.Result, &r); err != nil {
		return ""
	}
	return strings.ToLower(r.Code)
}

// referenceTime bestimmt, welchem Tag ein Task zugeordnet wird:
// Startzeit (nächtliches Backup 23:30–01:00 zählt zum Starttag), sonst Einreihung, sonst Abschluss.
func (t Task) referenceTime() time.Time {
	switch {
	case !t.StartedAt.IsZero():
		return t.StartedAt.Time
	case !t.EnqueuedAt.IsZero():
		return t.EnqueuedAt.Time
	default:
		return t.CompletedAt.Time
	}
}

// diagTask ist ein Task von gestern, wie er im Protokoll (backup_details) landet.
type diagTask struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	Title       string          `json:"title,omitempty"`
	State       string          `json:"state"`
	Result      string          `json:"result,omitempty"`
	StartedAt   string          `json:"started_at,omitempty"`
	CompletedAt string          `json:"completed_at,omitempty"`
	Counted     bool            `json:"counted_as_backup"`
	Error       string          `json:"error,omitempty"`
	RawError    json.RawMessage `json:"raw_error,omitempty"`

	refTime   time.Time
	isError   bool
	isSuccess bool
}

func (t Task) contextTitle() string {
	if len(t.Context) == 0 {
		return ""
	}
	var ctx map[string]interface{}
	if json.Unmarshal(t.Context, &ctx) != nil {
		return ""
	}
	title, _ := ctx["title"].(string)
	return title
}

// rawError liefert das Fehlerobjekt des Tasks (result.error, sonst das Top-Level-Feld error).
func (t Task) rawError() json.RawMessage {
	if len(t.Result) > 0 {
		var r taskResult
		if json.Unmarshal(t.Result, &r) == nil && len(r.Error) > 0 && string(r.Error) != "null" {
			return r.Error
		}
	}
	if t.Error != nil {
		b, _ := json.Marshal(t.Error)
		return b
	}
	return nil
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// errorSummary macht aus einem verschachtelten Fehler-JSON eine lesbare Zeile,
// z. B. "Backup/DiskFull: Nicht genug Speicherplatz → Storage/QuotaExceeded".
func errorSummary(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		return truncate(string(raw), 500)
	}
	var parts []string
	collectErrorParts(v, &parts, 0)
	if len(parts) == 0 {
		return truncate(string(raw), 500)
	}
	var deduped []string
	for _, p := range parts {
		if len(deduped) == 0 || deduped[len(deduped)-1] != p {
			deduped = append(deduped, p)
		}
	}
	return truncate(strings.Join(deduped, " → "), 500)
}

func collectErrorParts(v interface{}, parts *[]string, depth int) {
	if depth > 5 {
		return
	}
	switch x := v.(type) {
	case string:
		if x != "" {
			*parts = append(*parts, x)
		}
	case []interface{}:
		for _, item := range x {
			collectErrorParts(item, parts, depth+1)
		}
	case map[string]interface{}:
		msg := ""
		for _, k := range []string{"message", "reason", "description", "text", "title"} {
			if m, ok := x[k].(string); ok && m != "" {
				msg = m
				break
			}
		}
		label := ""
		domain, _ := x["domain"].(string)
		code := ""
		if c, ok := x["code"]; ok && c != nil {
			code = fmt.Sprintf("%v", c)
		}
		switch {
		case domain != "" && code != "":
			label = domain + "/" + code
		default:
			label = domain + code
		}
		entry := msg
		if label != "" && msg != "" {
			entry = label + ": " + msg
		} else if label != "" {
			entry = label
		}
		if entry != "" {
			*parts = append(*parts, entry)
		}
		for _, k := range []string{"cause", "causes", "error", "errors", "inner", "details"} {
			if nested, ok := x[k]; ok && nested != nil {
				if _, isString := nested.(string); !isString {
					collectErrorParts(nested, parts, depth+1)
				}
			}
		}
	}
}

func formatTaskRef(t *diagTask) string {
	ref := fmt.Sprintf("Task %d", t.ID)
	if t.Type != "" {
		ref += ", " + t.Type
	}
	if !t.refTime.IsZero() {
		ref += ", Start " + t.refTime.In(time.Local).Format("02.01. 15:04")
	}
	return "[" + ref + "]"
}

// diagnoseBackup erklärt, warum eine Maschine gestern kein erfolgreiches Backup hatte.
// dayLabel ist z. B. "Gestern" oder "Am 07.09." und leitet den Satz ein.
func diagnoseBackup(dayLabel string, tasks []diagTask, agentKnown, agentOnline bool, lastSuccess time.Time, windowStart time.Time) string {
	var backups []diagTask
	otherTypes := make(map[string]bool)
	for _, t := range tasks {
		if t.Counted {
			backups = append(backups, t)
		} else {
			otherTypes[t.Type] = true
		}
	}

	var reason string
	if len(backups) == 0 {
		if len(otherTypes) > 0 {
			types := make([]string, 0, len(otherTypes))
			for t := range otherTypes {
				types = append(types, t)
			}
			sort.Strings(types)
			reason = dayLabel + " kein Backup-Task, nur andere Tasks (Typen: " + strings.Join(types, ", ") + ")"
		} else {
			reason = dayLabel + " wurde kein Backup-Task gestartet"
		}
	} else {
		// Pro Kategorie den jeweils letzten Task merken
		var failed, cancelled, running, other *diagTask
		pickLatest := func(cur **diagTask, t *diagTask) {
			if *cur == nil || t.refTime.After((*cur).refTime) {
				*cur = t
			}
		}
		for i := range backups {
			t := &backups[i]
			switch {
			case t.isError:
				pickLatest(&failed, t)
			case t.Result == "cancelled":
				pickLatest(&cancelled, t)
			case t.State != "completed":
				pickLatest(&running, t)
			default:
				pickLatest(&other, t)
			}
		}

		switch {
		case failed != nil:
			reason = fmt.Sprintf("Fehlgeschlagen (result=%s)", failed.Result)
			if failed.Error != "" {
				reason += ": " + failed.Error
			}
			reason += " " + formatTaskRef(failed)
		case cancelled != nil:
			reason = "Abgebrochen " + formatTaskRef(cancelled)
		case running != nil:
			reason = fmt.Sprintf("Nicht abgeschlossen (state=%s) %s", running.State, formatTaskRef(running))
		case other != nil:
			reason = fmt.Sprintf("Beendet ohne Erfolgsergebnis (result=%s) %s", other.Result, formatTaskRef(other))
		}
		if len(backups) > 1 {
			reason += fmt.Sprintf(" – %d Backup-Tasks an diesem Tag, keiner erfolgreich", len(backups))
		}
	}

	if agentKnown && !agentOnline {
		reason += "; Agent ist offline"
	}
	if !lastSuccess.IsZero() {
		reason += "; letztes erfolgreiches Backup: " + lastSuccess.In(time.Local).Format("02.01.2006 15:04")
	} else {
		reason += "; kein erfolgreiches Backup seit " + windowStart.Format("02.01.2006")
	}
	return reason
}

// ---------- Backup-Pläne: Soll-Tage aus dem Zeitplan ----------

// Plan-Status einer Maschine
const (
	planNone      = 0 // kein aktiver Backup-Plan oder Zeitplan ausgeschaltet
	planScheduled = 1 // aktiver Plan mit auswertbarem Zeitplan
	planUnknown   = 2 // Zeitplan nicht auswertbar oder Pläne nicht abrufbar
)

type backupPlan struct {
	ID              string
	Name            string
	Enabled         bool
	ScheduleEnabled bool
	HasSettings     bool
	Weekdays        map[time.Weekday]bool
	Times           []string
	UnknownSchedule []string
}

type machineSchedule struct {
	State    int
	Weekdays map[time.Weekday]bool
	Info     []string
}

var weekdayByShort = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

func weekdayLabel(days map[time.Weekday]bool) string {
	if len(days) == 7 {
		return "täglich"
	}
	names := []string{"Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"}
	order := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	var out []string
	for i, d := range order {
		if days[d] {
			out = append(out, names[i])
		}
	}
	return strings.Join(out, ", ")
}

func (p backupPlan) describe() string {
	desc := fmt.Sprintf("Plan %q: %s", p.Name, weekdayLabel(p.Weekdays))
	if len(p.Times) > 0 {
		desc += " " + strings.Join(p.Times, ", ")
	}
	return desc
}

func boolField(m map[string]interface{}, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

// parsePlanItem liest einen Schutzplan (Eintrag mit der Liste "policy": Hauptplan und Unter-Richtlinien).
// Liefert false, wenn der Plan keine Backup-Richtlinie enthält.
func parsePlanItem(item map[string]interface{}) (backupPlan, bool) {
	var plan backupPlan
	policies, _ := item["policy"].([]interface{})
	var root, backup map[string]interface{}
	for _, raw := range policies {
		pol, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch pol["type"] {
		case "policy.protection.total":
			root = pol
		case "policy.backup.machine":
			backup = pol
		}
	}
	if backup == nil {
		return plan, false
	}
	if root == nil {
		root = backup
	}

	plan.ID, _ = root["id"].(string)
	plan.Name, _ = root["name"].(string)
	if plan.Name == "" {
		plan.Name = plan.ID
	}
	plan.Enabled = boolField(root, "enabled", true) && boolField(backup, "enabled", true)
	plan.Weekdays = make(map[time.Weekday]bool)

	settings, _ := backup["settings"].(map[string]interface{})
	scheduling, _ := settings["scheduling"].(map[string]interface{})
	if scheduling == nil {
		return plan, true
	}
	plan.HasSettings = true
	plan.ScheduleEnabled = boolField(scheduling, "enabled", true)

	sets, _ := scheduling["backup_sets"].([]interface{})
	seenTimes := make(map[string]bool)
	for _, rawSet := range sets {
		set, _ := rawSet.(map[string]interface{})
		schedule, _ := set["schedule"].(map[string]interface{})
		schedType, _ := schedule["type"].(string)
		alarms, _ := schedule["alarms"].(map[string]interface{})
		timeCfg, _ := alarms["time"].(map[string]interface{})
		days, _ := timeCfg["weekdays"].([]interface{})

		found := false
		for _, d := range days {
			if name, ok := d.(string); ok {
				if wd, ok := weekdayByShort[strings.ToLower(name)]; ok {
					plan.Weekdays[wd] = true
					found = true
				}
			}
		}
		if !found {
			if schedType == "" {
				schedType = "ohne Wochentage"
			}
			plan.UnknownSchedule = append(plan.UnknownSchedule, schedType)
			continue
		}
		repeat, _ := timeCfg["repeat_at"].([]interface{})
		for _, r := range repeat {
			if at, ok := r.(map[string]interface{}); ok {
				h, _ := at["hour"].(float64)
				m, _ := at["minute"].(float64)
				t := fmt.Sprintf("%02d:%02d", int(h), int(m))
				if !seenTimes[t] {
					seenTimes[t] = true
					plan.Times = append(plan.Times, t)
				}
			}
		}
	}
	return plan, true
}

// getPaged ruft einen Endpunkt ab und folgt paging.cursors.after.
func (c *AcronisClient) getPaged(endpoint string, params url.Values) ([]map[string]interface{}, [][]byte, error) {
	var items []map[string]interface{}
	var pages [][]byte
	cursor := ""
	for page := 0; page < 100; page++ {
		q := url.Values{}
		for k, v := range params {
			q[k] = v
		}
		if cursor != "" {
			q.Set("after", cursor)
		}
		data, err := c.doRequest("GET", endpoint+"?"+q.Encode())
		if err != nil {
			return items, pages, err
		}
		pages = append(pages, data)
		var resp struct {
			Items  []map[string]interface{} `json:"items"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
			} `json:"paging"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return items, pages, fmt.Errorf("Antwort von %s konnte nicht geparst werden: %w", endpoint, err)
		}
		items = append(items, resp.Items...)
		if resp.Paging.Cursors.After == "" || len(resp.Items) == 0 || resp.Paging.Cursors.After == cursor {
			break
		}
		cursor = resp.Paging.Cursors.After
	}
	return items, pages, nil
}

// fetchBackupPlans holt alle Schutzpläne mit Backup-Richtlinie und die Ressourcen, auf die sie angewendet sind.
func (c *AcronisClient) fetchBackupPlans() ([]backupPlan, map[string][]string, [][]byte, error) {
	items, pages, err := c.getPaged("/api/policy_management/v4/policies", url.Values{"include_settings": {"true"}})
	if err != nil {
		return nil, nil, pages, err
	}

	var plans []backupPlan
	contexts := make(map[string][]string)
	for _, item := range items {
		plan, ok := parsePlanItem(item)
		if !ok || plan.ID == "" {
			continue
		}
		plans = append(plans, plan)

		ctxItems, ctxPages, err := c.getPaged("/api/policy_management/v4/policies",
			url.Values{"policy_id": {plan.ID}, "include_applied_context": {"true"}})
		pages = append(pages, ctxPages...)
		if err != nil {
			return plans, contexts, pages, fmt.Errorf("Zuordnung für Plan %q: %w", plan.Name, err)
		}
		for _, ci := range ctxItems {
			ctx, _ := ci["context"].(map[string]interface{})
			ids, _ := ctx["items"].([]interface{})
			for _, id := range ids {
				if s, ok := id.(string); ok && s != "" {
					contexts[plan.ID] = append(contexts[plan.ID], strings.ToLower(s))
				}
			}
		}
	}
	return plans, contexts, pages, nil
}

// buildSchedules bestimmt pro Maschine den gültigen Zeitplan.
func buildSchedules(plans []backupPlan, contexts map[string][]string, fetchErr error,
	machineNames map[string]string, allResources map[string]ResourceItem) map[string]*machineSchedule {

	result := make(map[string]*machineSchedule)
	if fetchErr != nil {
		for id := range machineNames {
			result[id] = &machineSchedule{State: planUnknown, Info: []string{"Backup-Pläne konnten nicht abgerufen werden: " + truncate(fetchErr.Error(), 200)}}
		}
		return result
	}

	plansByMachine := make(map[string][]backupPlan)
	var groupPlans []string
	for _, plan := range plans {
		for _, id := range contexts[plan.ID] {
			if _, isMachine := machineNames[id]; isMachine {
				plansByMachine[id] = append(plansByMachine[id], plan)
			} else if plan.Enabled {
				label := id
				if r, ok := allResources[id]; ok {
					label = fmt.Sprintf("%s (%s)", r.Name, r.Type)
				}
				groupPlans = append(groupPlans, fmt.Sprintf("Plan %q auf %s", plan.Name, label))
			}
		}
	}

	for id := range machineNames {
		ms := &machineSchedule{Weekdays: make(map[time.Weekday]bool)}
		result[id] = ms
		assigned := plansByMachine[id]

		if len(assigned) == 0 {
			if len(groupPlans) > 0 {
				ms.State = planUnknown
				ms.Info = []string{"Kein direkt zugewiesener Plan; Pläne auf Gruppen oder anderen Ressourcen können nicht zugeordnet werden"}
			} else {
				ms.State = planNone
				ms.Info = []string{"Kein Backup-Plan zugewiesen"}
			}
			continue
		}

		active, unknown := 0, 0
		for _, plan := range assigned {
			switch {
			case !plan.Enabled:
				ms.Info = append(ms.Info, fmt.Sprintf("Plan %q ist deaktiviert", plan.Name))
			case !plan.HasSettings:
				unknown++
				ms.Info = append(ms.Info, fmt.Sprintf("Plan %q: Einstellungen nicht verfügbar", plan.Name))
			case !plan.ScheduleEnabled:
				ms.Info = append(ms.Info, fmt.Sprintf("Plan %q: Zeitplan ausgeschaltet (nur manueller Start)", plan.Name))
			case len(plan.Weekdays) == 0:
				unknown++
				ms.Info = append(ms.Info, fmt.Sprintf("Plan %q: Zeitplan-Typ %s wird nicht ausgewertet", plan.Name, strings.Join(plan.UnknownSchedule, ", ")))
			default:
				active++
				for d := range plan.Weekdays {
					ms.Weekdays[d] = true
				}
				desc := plan.describe()
				if len(plan.UnknownSchedule) > 0 {
					desc += " (weitere Zeitpläne nicht ausgewertet: " + strings.Join(plan.UnknownSchedule, ", ") + ")"
				}
				ms.Info = append(ms.Info, desc)
			}
		}
		switch {
		case active > 0:
			ms.State = planScheduled
		case unknown > 0:
			ms.State = planUnknown
		default:
			ms.State = planNone
		}
	}
	return result
}

// expectedBackupDates liefert die Tage im Fenster, an denen laut Zeitplan ein Backup laufen soll.
// Tage vor dem Anlegen der Maschine zählen nicht.
func expectedBackupDates(ms *machineSchedule, from, to time.Time, created time.Time) []time.Time {
	var dates []time.Time
	if ms == nil || ms.State != planScheduled {
		return dates
	}
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		if !created.IsZero() && !created.Before(day.AddDate(0, 0, 1)) {
			continue
		}
		if ms.Weekdays[day.Weekday()] {
			dates = append(dates, day)
		}
	}
	return dates
}

// countFulfilledDays zählt Soll-Tage mit erfolgreichem Backup. Ein Backup zählt für seinen Tag oder,
// wenn es innerhalb der Karenzzeit nach Mitternacht startet (verspätet, Wiederholung), für den Vortag.
// Jedes Backup erfüllt höchstens einen Soll-Tag.
func countFulfilledDays(expected []time.Time, successes []time.Time, grace time.Duration) int {
	sorted := append([]time.Time(nil), successes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	used := make([]bool, len(sorted))
	fulfilled := 0
	for _, day := range expected {
		end := day.AddDate(0, 0, 1).Add(grace)
		for i, t := range sorted {
			if !used[i] && !t.Before(day) && t.Before(end) {
				used[i] = true
				fulfilled++
				break
			}
		}
	}
	return fulfilled
}

const weeklyPlanMigrationSQL = `ALTER TABLE acronis_weekly_reports
	ADD COLUMN IF NOT EXISTS expected_backup_days INT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS fulfilled_backup_days INT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS plan_state INT NOT NULL DEFAULT 2,
	ADD COLUMN IF NOT EXISTS schedule_info TEXT NOT NULL DEFAULT ''`

func ensureWeeklyPlanColumns(db *sql.DB) error {
	return ensureColumns(db, "acronis_weekly_reports",
		[]string{"expected_backup_days", "fulfilled_backup_days", "plan_state", "schedule_info"}, weeklyPlanMigrationSQL)
}

// dayData sammelt die Backup-Ergebnisse eines vergangenen Tages für das Nachtragen.
type dayData struct {
	tasks   map[string][]diagTask
	success map[string]bool
	errors  map[string]bool
}

func newDayData() *dayData {
	return &dayData{tasks: make(map[string][]diagTask), success: make(map[string]bool), errors: make(map[string]bool)}
}

func lastSuccessBefore(times []time.Time, limit time.Time) time.Time {
	var last time.Time
	for _, t := range times {
		if t.Before(limit) && t.After(last) {
			last = t
		}
	}
	return last
}

// backfillDailyReports schreibt Tagesreports für Tage, an denen der Collector nicht gelaufen ist.
// Bestehende Reports werden nie verändert. Eine Maschine bekommt einen nachgetragenen Tag nur, wenn sie
// an diesem Tag nachweislich schon existierte: früherer Report, Anlage vor dem Tag oder ein Task an dem Tag.
func backfillDailyReports(db *sql.DB, cloudName string, machineNames map[string]string, created map[string]time.Time,
	pastDays map[string]*dayData, successTimes map[string][]time.Time, windowStart, startOfYesterday time.Time, days int) (int, []string, error) {

	first := startOfYesterday.AddDate(0, 0, -days)
	if first.Before(windowStart) {
		first = windowStart
	}
	firstStr := first.Format("2006-01-02")
	yesterdayStr := startOfYesterday.Format("2006-01-02")

	existing := make(map[string]bool)
	rows, err := db.Query(`
		SELECT machine_id, report_date::date::text FROM acronis_daily_reports
		WHERE cloud_name = $1 AND report_date::date >= $2::date AND report_date::date < $3::date
	`, cloudName, firstStr, yesterdayStr)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var id, date string
		if rows.Scan(&id, &date) == nil {
			existing[strings.ToLower(id)+"|"+date] = true
		}
	}
	rows.Close()

	firstReport := make(map[string]string)
	rows, err = db.Query(`SELECT machine_id, MIN(report_date::date)::text FROM acronis_daily_reports WHERE cloud_name = $1 GROUP BY machine_id`, cloudName)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var id, date string
		if rows.Scan(&id, &date) == nil {
			firstReport[strings.ToLower(id)] = date
		}
	}
	rows.Close()

	filled := 0
	var filledDates []string
	note := "; nachgetragen am " + time.Now().Format("02.01.2006")

	for day := first; day.Before(startOfYesterday); day = day.AddDate(0, 0, 1) {
		dayStr := day.Format("2006-01-02")
		nextDay := day.AddDate(0, 0, 1)
		data := pastDays[dayStr]
		if data == nil {
			data = newDayData()
		}
		dayFilled := 0

		for machineID, name := range machineNames {
			if existing[machineID+"|"+dayStr] {
				continue
			}
			fr, hasEarlierReport := firstReport[machineID]
			c, hasCreated := created[machineID]
			existedThatDay := (hasEarlierReport && fr < dayStr) ||
				(hasCreated && c.Before(day)) ||
				len(data.tasks[machineID]) > 0
			if !existedThatDay {
				continue
			}

			details := data.tasks[machineID]
			sort.Slice(details, func(i, j int) bool { return details[i].refTime.Before(details[j].refTime) })
			if details == nil {
				details = []diagTask{}
			}
			detailsJSON, _ := json.Marshal(details)

			success := data.success[machineID]
			reason := ""
			if !success {
				// Agent-Status von heute sagt nichts über den damaligen Tag aus, daher nicht verwenden
				reason = diagnoseBackup("Am "+day.Format("02.01."), details, false, false, lastSuccessBefore(successTimes[machineID], nextDay), windowStart) + note
			}

			res, err := db.Exec(`
				INSERT INTO acronis_daily_reports (cloud_name, machine_id, machine_name, backup_successful, has_errors, report_date, failure_reason, backup_details, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, NOW())
				ON CONFLICT (cloud_name, machine_id, report_date) DO NOTHING
			`, cloudName, machineID, name, success, data.errors[machineID], dayStr, reason, string(detailsJSON))
			if err != nil {
				return filled, filledDates, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				filled++
				dayFilled++
				// Ab jetzt gilt die Maschine auch für die Folgetage als vorhanden
				if !hasEarlierReport || dayStr < fr {
					firstReport[machineID] = dayStr
				}
			}
		}
		if dayFilled > 0 {
			filledDates = append(filledDates, day.Format("02.01."))
		}
	}
	return filled, filledDates, nil
}

// agentRecord ist ein Agent aus dem Agent Manager, inklusive Herkunft des Online-Status.
type agentRecord struct {
	ID               string
	Hostname         string
	Online           bool
	BuildVersion     string
	ReleaseID        string
	InstallerVersion string
	// Source: api, recheck (zweite Abfrage), task_activity, offline_confirmed, offline_unconfirmed
	Source       string
	LastActivity time.Time
	firstOffline bool
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		fmt.Printf("WARNUNG: %s=%q ist keine gültige Zahl, verwende %d\n", key, v, def)
	}
	return def
}

func (c *AcronisClient) fetchAgents() ([]json.RawMessage, [][]byte, error) {
	var all []json.RawMessage
	var pages [][]byte
	cursor := ""
	for page := 0; page < 100; page++ {
		q := url.Values{}
		q.Add("limit", "1000")
		if cursor != "" {
			q.Add("after", cursor)
		}
		data, err := c.doRequest("GET", "/api/agent_manager/v2/agents?"+q.Encode())
		if err != nil {
			return all, pages, err
		}
		pages = append(pages, data)

		var response struct {
			Items  []json.RawMessage `json:"items"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
			} `json:"paging"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return all, pages, fmt.Errorf("Agents konnten nicht geparst werden: %w", err)
		}
		all = append(all, response.Items...)
		if response.Paging.Cursors.After == "" || len(response.Items) == 0 || response.Paging.Cursors.After == cursor {
			break
		}
		cursor = response.Paging.Cursors.After
	}
	return all, pages, nil
}

func parseAgent(raw json.RawMessage) (*agentRecord, bool) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}

	id := ""
	if v, ok := m["id"].(string); ok {
		id = v
	}
	for _, k := range []string{"resource_id", "machine_id", "host_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			id = v
			break
		}
	}
	if id == "" {
		return nil, false
	}

	a := &agentRecord{ID: strings.ToLower(id), Hostname: "Unbekannt", Source: "api"}
	if h, ok := m["hostname"].(string); ok && h != "" {
		a.Hostname = h
	}
	if online, ok := m["online"].(bool); ok {
		a.Online = online
	}
	a.firstOffline = !a.Online

	if coreVer, ok := m["core_version"].(map[string]interface{}); ok {
		if current, ok := coreVer["current"].(map[string]interface{}); ok {
			if v, ok := current["build"]; ok && v != nil {
				a.BuildVersion = fmt.Sprintf("%v", v)
			}
			if v, ok := current["release_id"]; ok && v != nil {
				a.ReleaseID = fmt.Sprintf("%v", v)
			}
		}
	}
	if instVer, ok := m["installer_version"].(map[string]interface{}); ok {
		if current, ok := instVer["current"].(map[string]interface{}); ok {
			if v, ok := current["release_id"]; ok && v != nil {
				a.InstallerVersion = fmt.Sprintf("%v", v)
			}
		}
	}
	return a, true
}

// recheckOfflineAgents fragt die Agent-Liste nach einer Wartezeit erneut ab.
// Ein Agent, der jetzt online ist, war nur kurz getrennt und wird als online gespeichert.
func recheckOfflineAgents(client *AcronisClient, agents []*agentRecord, delay time.Duration) {
	offline := 0
	for _, a := range agents {
		if !a.Online {
			offline++
		}
	}
	if offline == 0 || delay <= 0 {
		return
	}

	fmt.Printf("-> %d Agents als offline gemeldet, erneute Abfrage in %s ...\n", offline, delay)
	time.Sleep(delay)

	raw, _, err := client.fetchAgents()
	if err != nil {
		fmt.Printf("WARNUNG: Erneute Agent-Abfrage fehlgeschlagen, verwende erste Abfrage: %v\n", err)
		return
	}
	second := make(map[string]bool)
	for _, r := range raw {
		if a, ok := parseAgent(r); ok {
			second[a.ID] = second[a.ID] || a.Online
		}
	}
	for _, a := range agents {
		if !a.Online && second[a.ID] {
			a.Online = true
			a.Source = "recheck"
		}
	}
}

func shortHostname(name string) string {
	return strings.SplitN(strings.ToLower(strings.TrimSpace(name)), ".", 2)[0]
}

// applyTaskActivity wertet Agents als online, wenn ihre Maschine im Zeitfenster nachweislich gearbeitet hat:
// ein laufender Task mit aktuellem Fortschritt oder ein erfolgreich abgeschlossener Task.
// Serverseitig beendete Tasks (abandoned, timedout) zählen nicht, die entstehen gerade bei nicht erreichbaren Agents.
func applyTaskActivity(agents []*agentRecord, tasks []Task, machineNames map[string]string, window time.Duration) {
	cutoff := time.Now().Add(-window)
	activity := make(map[string]time.Time)
	for _, t := range tasks {
		resourceID := ""
		if t.Resource != nil {
			resourceID = t.Resource.ID
		}
		if resourceID == "" {
			resourceID = t.ResourceID
		}
		if resourceID == "" {
			continue
		}
		resourceID = strings.ToLower(resourceID)

		var seen time.Time
		code := t.resultCode()
		switch {
		case t.State == "started" || t.State == "paused":
			seen = t.UpdatedAt.Time
		case t.State == "completed" && (code == "ok" || code == "warning"):
			seen = t.CompletedAt.Time
		}
		if seen.After(activity[resourceID]) {
			activity[resourceID] = seen
		}
	}

	// Zuordnung Agent -> Ressource: direkt über die ID, sonst über den Hostnamen
	idsByHost := make(map[string][]string)
	for id, name := range machineNames {
		idsByHost[shortHostname(name)] = append(idsByHost[shortHostname(name)], id)
	}

	for _, a := range agents {
		last := activity[a.ID]
		for _, id := range idsByHost[shortHostname(a.Hostname)] {
			if activity[id].After(last) {
				last = activity[id]
			}
		}
		a.LastActivity = last

		if a.Online {
			continue
		}
		if window > 0 && !last.IsZero() && last.After(cutoff) {
			a.Online = true
			a.Source = "task_activity"
			continue
		}
		if a.Source == "api" && window > 0 {
			a.Source = "offline_confirmed"
		} else if a.Source == "api" {
			a.Source = "offline_unconfirmed"
		}
	}
}

func logAgentStatus(agents []*agentRecord) {
	var corrected, offline []string
	for _, a := range agents {
		last := "keine Task-Aktivität im abgefragten Zeitraum"
		if !a.LastActivity.IsZero() {
			last = "letzte Task-Aktivität " + a.LastActivity.In(time.Local).Format("02.01. 15:04")
		}
		switch {
		case a.firstOffline && a.Source == "recheck":
			corrected = append(corrected, fmt.Sprintf("%s: bei der zweiten Abfrage online", a.Hostname))
		case a.firstOffline && a.Source == "task_activity":
			corrected = append(corrected, fmt.Sprintf("%s: als online gewertet, %s", a.Hostname, last))
		case !a.Online:
			offline = append(offline, fmt.Sprintf("%s (%s)", a.Hostname, last))
		}
	}
	sort.Strings(corrected)
	sort.Strings(offline)
	if len(corrected) > 0 {
		fmt.Printf("-> %d Agents zunächst offline gemeldet, nach Gegenprüfung online:\n", len(corrected))
		for _, c := range corrected {
			fmt.Printf("   %s\n", c)
		}
	}
	if len(offline) > 0 {
		fmt.Printf("-> %d Agents offline bestätigt:\n", len(offline))
		for _, o := range offline {
			fmt.Printf("   %s\n", o)
		}
	}
}

// ensureColumns prüft Spalten der Tabelle, auf die sich der unqualifizierte Name auflöst, und legt fehlende an.
func ensureColumns(db *sql.DB, table string, columns []string, migration string) error {
	var n int
	var resolved, database string
	err := db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM pg_attribute
			 WHERE attrelid = to_regclass($1)
			   AND attname = ANY($2::text[])
			   AND NOT attisdropped),
			COALESCE(to_regclass($1)::text, ''),
			current_database()
	`, table, "{"+strings.Join(columns, ",")+"}").Scan(&n, &resolved, &database)
	if err != nil {
		return err
	}
	if resolved == "" {
		return fmt.Errorf("Tabelle %s nicht gefunden (Datenbank %s)", table, database)
	}
	if n == len(columns) {
		return nil
	}
	if _, err := db.Exec(migration); err != nil {
		return err
	}
	fmt.Printf("Spalten %s in %s angelegt (Datenbank %s)\n", strings.Join(columns, ", "), resolved, database)
	return nil
}

const agentMigrationSQL = `ALTER TABLE acronis_agents
	ADD COLUMN IF NOT EXISTS online_source TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ`

func ensureAgentColumns(db *sql.DB) error {
	return ensureColumns(db, "acronis_agents", []string{"online_source", "last_seen_at"}, agentMigrationSQL)
}

// ensureDailyReportColumns legt die Protokoll-Spalten an, falls sie fehlen.
// Geprüft wird genau die Tabelle, auf die sich der unqualifizierte Name auflöst (search_path),
// damit eine gleichnamige Tabelle in einem anderen Schema nicht fälschlich als "vorhanden" zählt.
func ensureDailyReportColumns(db *sql.DB) error {
	var n int
	var table, database string
	err := db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM pg_attribute
			 WHERE attrelid = to_regclass('acronis_daily_reports')
			   AND attname IN ('failure_reason', 'backup_details')
			   AND NOT attisdropped),
			COALESCE(to_regclass('acronis_daily_reports')::text, ''),
			current_database()
	`).Scan(&n, &table, &database)
	if err != nil {
		return err
	}
	if table == "" {
		return fmt.Errorf("Tabelle acronis_daily_reports nicht gefunden (Datenbank %s)", database)
	}
	if n == 2 {
		fmt.Printf("Backup-Protokoll: Spalten vorhanden in %s (Datenbank %s)\n", table, database)
		return nil
	}
	if _, err := db.Exec(dailyReportMigrationSQL); err != nil {
		return err
	}
	fmt.Printf("Backup-Protokoll: Spalten angelegt in %s (Datenbank %s)\n", table, database)
	return nil
}

const dailyReportMigrationSQL = `ALTER TABLE acronis_daily_reports
	ADD COLUMN IF NOT EXISTS failure_reason TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS backup_details JSONB`

// parseBackupTaskTypes liest optional ACRONIS_BACKUP_TASK_TYPES (kommagetrennt).
// Leer = alle Task-Typen zählen (bisheriges Verhalten).
func parseBackupTaskTypes() map[string]bool {
	raw := os.Getenv("ACRONIS_BACKUP_TASK_TYPES")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	types := make(map[string]bool)
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			types[strings.ToLower(t)] = true
		}
	}
	return types
}

type Activity struct {
	ID        int64       `json:"id"` // <- Hier wurde von string auf int64 gewechselt
	Type      string      `json:"type"`
	State     string      `json:"state"`
	Status    string      `json:"status"`
	TaskID    *int64      `json:"task_id"`
	TenantID  string      `json:"tenant_id"`
	Context   interface{} `json:"context"`
	CreatedAt time.Time   `json:"created_at"`
}

func NewAcronisClient(baseURL, username, password string) *AcronisClient {
	return &AcronisClient{
		baseURL:  baseURL,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

func (c *AcronisClient) Login() error {
	data := url.Values{}
	data.Set("grant_type", "password")
	data.Set("username", c.username)
	data.Set("password", c.password)

	req, err := http.NewRequest("POST", c.baseURL+"/api/2/idp/token", strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login fehlgeschlagen, status: %d, details: %s", resp.StatusCode, string(bodyBytes))
	}

	var auth struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		return err
	}

	c.token = auth.AccessToken
	return nil
}

func (c *AcronisClient) doRequest(method, endpoint string) ([]byte, error) {
	req, err := http.NewRequest(method, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API fehler bei %s, status: %d, details: %s", endpoint, resp.StatusCode, string(bodyBytes))
	}

	return io.ReadAll(resp.Body)
}

// loadEnvFile sucht die .env im Arbeitsverzeichnis und neben der ausführbaren Datei
// und lädt die Variablen daraus. Bereits gesetzte Umgebungsvariablen haben Vorrang.
func loadEnvFile(filename string) {
	candidates := []string{filename}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), filename))
	}

	for _, path := range candidates {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		absPath, _ := filepath.Abs(path)

		// PowerShell (echo / Out-File) speichert standardmäßig als UTF-16, das kann nicht gelesen werden
		if bytes.HasPrefix(content, []byte{0xFF, 0xFE}) || bytes.HasPrefix(content, []byte{0xFE, 0xFF}) {
			fmt.Printf("WARNUNG: %s ist UTF-16 kodiert und wird ignoriert. Bitte als UTF-8 speichern.\n", absPath)
			continue
		}

		// UTF-8-BOM entfernen (Editor unter Windows), sonst wird der erste Schlüssel nicht erkannt
		content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})

		loaded := 0
		scanner := bufio.NewScanner(bytes.NewReader(content))
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
				loaded++
			}
		}

		fmt.Printf(".env geladen: %s (%d Einträge)\n", absPath, loaded)
		return
	}

	wd, _ := os.Getwd()
	fmt.Printf("WARNUNG: Keine %s gefunden. Gesucht im Arbeitsverzeichnis (%s) und neben der ausführbaren Datei.\n", filename, wd)
}

func fetchCloudsFromKeycloak() []CloudConfig {
	serverURL := os.Getenv("KEYCLOAK_SERVER_URL")
	realm := os.Getenv("KEYCLOAK_REALM")
	clientID := os.Getenv("KEYCLOAK_CLIENT_ID")
	clientSecret := os.Getenv("KEYCLOAK_CLIENT_SECRET")

	if serverURL == "" || realm == "" || clientID == "" || clientSecret == "" {
		panic("Keycloak Umgebungsvariablen (SERVER_URL, REALM, CLIENT_ID, CLIENT_SECRET) sind nicht vollständig gesetzt.")
	}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", serverURL, realm)
	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	reqToken, err := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		panic(fmt.Sprintf("Fehler beim Erstellen des Token-Requests: %v", err))
	}
	reqToken.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqToken.SetBasicAuth(clientID, clientSecret)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	resp, err := client.Do(reqToken)
	if err != nil {
		panic(fmt.Sprintf("Fehler beim Verbinden mit Keycloak für den Token-Abruf: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		panic(fmt.Sprintf("Keycloak Token-Fehler, Status: %d, Details: %s", resp.StatusCode, string(bodyBytes)))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		panic(fmt.Sprintf("Fehler beim Parsen des Keycloak-Tokens: %v", err))
	}

	adminURL := fmt.Sprintf("%s/admin/realms/%s/clients", serverURL, realm)
	req, err := http.NewRequest("GET", adminURL, nil)
	if err != nil {
		panic(fmt.Sprintf("Fehler beim Erstellen des Admin-Requests: %v", err))
	}
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)

	adminResp, err := client.Do(req)
	if err != nil {
		panic(fmt.Sprintf("Fehler beim Abrufen der Clients aus Keycloak: %v", err))
	}
	defer adminResp.Body.Close()

	if adminResp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(adminResp.Body)
		panic(fmt.Sprintf("Keycloak Admin API Fehler, Status: %d, Details: %s", adminResp.StatusCode, string(bodyBytes)))
	}

	var kcClients []struct {
		ClientID   string            `json:"clientId"`
		Attributes map[string]string `json:"attributes"`
	}

	if err := json.NewDecoder(adminResp.Body).Decode(&kcClients); err != nil {
		panic(fmt.Sprintf("Fehler beim Parsen der Keycloak-Clients: %v", err))
	}

	var clouds []CloudConfig
	for _, kc := range kcClients {
		if strings.HasPrefix(kc.ClientID, "account") || strings.HasPrefix(kc.ClientID, "admin") || strings.HasPrefix(kc.ClientID, "broker") || strings.HasPrefix(kc.ClientID, "realm") {
			continue
		}

		baseURL := kc.Attributes["base_url"]
		username := kc.Attributes["username"]
		password := kc.Attributes["password"]

		if baseURL != "" && username != "" && password != "" {
			clouds = append(clouds, CloudConfig{
				Name:     kc.ClientID,
				BaseURL:  baseURL,
				Username: username,
				Password: password,
			})
		}
	}

	return clouds
}

func main() {
	loadEnvFile(".env")
	val := os.Getenv("APP_DEBUG")
	GetDebug, err := strconv.ParseBool(val)
	if err != nil {
		GetDebug = false
	}

	if GetDebug {
		fmt.Printf("Der Debug-Modus ist aktiviert und protokolliert alles im Raw-JSON-Format unter acronis_raw_logs\n")
	} else {
		fmt.Printf("Der Debug-Modus ist Aus \n")
	}

	connStr := os.Getenv("DB_CONNECTION_STRING")
	if connStr == "" {
		panic("DB_CONNECTION_STRING ist nicht gesetzt")
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		panic(fmt.Sprintf("DB Verbindungsfehler: %v", err))
	}
	defer db.Close()

	if err := ensureWeeklyPlanColumns(db); err != nil {
		panic(fmt.Sprintf("Spalten für den Soll/Ist-Vergleich fehlen in acronis_weekly_reports und konnten nicht angelegt werden: %v\nBitte manuell ausführen:\n%s;", err, weeklyPlanMigrationSQL))
	}
	if err := ensureAgentColumns(db); err != nil {
		panic(fmt.Sprintf("Spalten für die Online-Prüfung fehlen in acronis_agents und konnten nicht angelegt werden: %v\nBitte manuell ausführen:\n%s;", err, agentMigrationSQL))
	}
	if err := ensureDailyReportColumns(db); err != nil {
		panic(fmt.Sprintf("Spalten für das Backup-Protokoll fehlen und konnten nicht angelegt werden: %v\nBitte manuell ausführen:\n%s;", err, dailyReportMigrationSQL))
	}

	clouds := fetchCloudsFromKeycloak()

	now := time.Now()
	sevenDaysAgo := now.AddDate(0, 0, -7)
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	startOfYesterday := startOfToday.AddDate(0, 0, -1)
	// Wochenreport = die letzten 7 vollständigen Kalendertage (ohne heute).
	// Ein gleitendes Fenster (jetzt minus 7 Tage) liefert je nach Uhrzeit 6 bis 8 Tage und lässt Trigger flattern.
	startOfWeekWindow := startOfToday.AddDate(0, 0, -7)
	reportDateYesterdayStr := startOfYesterday.Format("2006-01-02")
	backupTaskTypes := parseBackupTaskTypes()
	agentRecheckDelay := time.Duration(envInt("AGENT_RECHECK_SECONDS", 20)) * time.Second
	// Fehlende Tagesreports der letzten Tage nachtragen (maximal 7, so weit reichen die abgerufenen Tasks)
	// Verspätete Backups (z. B. Start nach Mitternacht) zählen noch für den geplanten Vortag
	scheduleGrace := time.Duration(envInt("SCHEDULE_GRACE_HOURS", 12)) * time.Hour
	backfillDays := envInt("BACKFILL_DAYS", 7)
	if backfillDays > 7 {
		backfillDays = 7
	}
	agentActivityWindow := time.Duration(envInt("AGENT_ACTIVITY_MINUTES", 30)) * time.Minute

	fmt.Printf("Zeitzone: %s, Tagesreport für %s (%s bis %s), Wochenreport ab %s\n", time.Local.String(), reportDateYesterdayStr,
		startOfYesterday.Format(time.RFC3339), startOfToday.Format(time.RFC3339), startOfWeekWindow.Format("2006-01-02"))

	for _, cloud := range clouds {
		fmt.Printf("Verarbeite Cloud: %s\n", cloud.Name)

		client := NewAcronisClient(cloud.BaseURL, cloud.Username, cloud.Password)
		if err := client.Login(); err != nil {
			fmt.Printf("Fehler beim Login für %s: %v\n", cloud.Name, err)
			continue
		}

		// Ressourcen, für die Wochen-/Tagesreports geschrieben werden.
		// Standard wie bisher "machine"; erweiterbar z. B. mit or(resource.machine,resource.virtual_machine.vmwesx)
		reportResourceTypes := strings.TrimSpace(os.Getenv("ACRONIS_REPORT_RESOURCE_TYPES"))
		if reportResourceTypes == "" {
			reportResourceTypes = "machine"
		}

		machineItems, resPages, err := client.fetchResources(reportResourceTypes)
		if GetDebug {
			for _, p := range resPages {
				db.Exec("INSERT INTO acronis_raw_logs (cloud_name, endpoint, raw_response) VALUES ($1, $2, $3)",
					cloud.Name, "resources", string(p))
			}
		}
		if err != nil {
			fmt.Printf("Fehler beim Abrufen der Ressourcen für %s: %v\n", cloud.Name, err)
			continue
		}
		machineNames := make(map[string]string)
		machineCreated := make(map[string]time.Time)
		for _, res := range machineItems {
			machineNames[strings.ToLower(res.ID)] = res.Name
			if !res.CreatedAt.IsZero() {
				machineCreated[strings.ToLower(res.ID)] = res.CreatedAt.Time
			}
		}
		fmt.Printf("-> %d Report-Ressourcen (Filter: %s): %s\n", len(machineNames), reportResourceTypes, resourceTypeSummary(machineItems))

		// Alle Ressourcen ohne Filter, nur um unbekannte resource.ids aus Tasks aufzulösen (Fehler hier sind nicht kritisch)
		allResources := make(map[string]ResourceItem)
		allItems, allPages, err := client.fetchResources("")
		if GetDebug {
			for _, p := range allPages {
				db.Exec("INSERT INTO acronis_raw_logs (cloud_name, endpoint, raw_response) VALUES ($1, $2, $3)",
					cloud.Name, "resources_all", string(p))
			}
		}
		if err != nil {
			fmt.Printf("WARNUNG: Alle Ressourcen konnten nicht abgerufen werden (nur für Diagnose): %v\n", err)
		} else {
			for _, res := range allItems {
				allResources[strings.ToLower(res.ID)] = res
			}
			fmt.Printf("-> %d Ressourcen insgesamt: %s\n", len(allItems), resourceTypeSummary(allItems))
		}

		fmt.Printf("Rufe Agents für %s ab (Pagination läuft)...\n", cloud.Name)
		rawAgents, agentPages, agentErr := client.fetchAgents()
		if GetDebug {
			for _, p := range agentPages {
				db.Exec("INSERT INTO acronis_raw_logs (cloud_name, endpoint, raw_response) VALUES ($1, $2, $3)",
					cloud.Name, "agents", string(p))
			}
		}
		if agentErr != nil {
			fmt.Printf("Fehler beim Abrufen der Agents: %v\n", agentErr)
		}

		var agents []*agentRecord
		for _, raw := range rawAgents {
			if a, ok := parseAgent(raw); ok {
				agents = append(agents, a)
			}
		}
		fmt.Printf("-> %d Agents insgesamt für %s heruntergeladen.\n", len(agents), cloud.Name)

		// Gegenprüfung 1: Als offline gemeldete Agents nach kurzer Wartezeit erneut abfragen
		recheckOfflineAgents(client, agents, agentRecheckDelay)
		agentOnline := make(map[string]bool)

		alertData, err := client.doRequest("GET", "/api/alert_manager/v1/alerts")
		if err == nil {
			if GetDebug {
				db.Exec("INSERT INTO acronis_raw_logs (cloud_name, endpoint, raw_response) VALUES ($1, $2, $3)",
					cloud.Name, "alerts", string(alertData))
			}
			var rawResponse struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(alertData, &rawResponse); err == nil {
				var fetchedIDs []string

				for _, rawItem := range rawResponse.Items {
					var alertMap map[string]interface{}
					if err := json.Unmarshal(rawItem, &alertMap); err != nil {
						continue
					}

					alertID, _ := alertMap["id"].(string)
					if alertID == "" {
						continue
					}

					fetchedIDs = append(fetchedIDs, alertID)

					alertType, _ := alertMap["type"].(string)
					severity, _ := alertMap["severity"].(string)

					resourceName := ""
					if detailsObj, ok := alertMap["details"].(map[string]interface{}); ok {
						if rn, ok := detailsObj["resourceName"].(string); ok {
							resourceName = rn
						}
					}
					if resourceName == "" {
						if rn, ok := alertMap["resource_name"].(string); ok {
							resourceName = rn
						}
					}

					tenantID := ""
					if tenantObj, ok := alertMap["tenant"].(map[string]interface{}); ok {
						if tUuid, ok := tenantObj["uuid"].(string); ok {
							tenantID = tUuid
						} else if tId, ok := tenantObj["id"].(string); ok {
							tenantID = fmt.Sprintf("%v", tId)
						}
					}
					if tenantID == "" {
						if tID, ok := alertMap["tenant_id"].(string); ok {
							tenantID = tID
						}
					}

					var createdAt time.Time
					for _, timeKey := range []string{"createdAt", "created_at"} {
						if caStr, ok := alertMap[timeKey].(string); ok && caStr != "" {
							if parsedTime, err := time.Parse(time.RFC3339, caStr); err == nil {
								createdAt = parsedTime
								break
							}
						}
					}

					detailsJSON, _ := json.Marshal(alertMap)

					_, dbErr := db.Exec(`
						INSERT INTO acronis_alerts (id, cloud_name, type, severity, created_at, resource_name, tenant_id, details) 
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8) 
						ON CONFLICT (id) DO UPDATE SET 
							severity = EXCLUDED.severity, 
							resource_name = EXCLUDED.resource_name,
							tenant_id = EXCLUDED.tenant_id,
							details = EXCLUDED.details
					`,
						alertID, cloud.Name, alertType, severity, createdAt,
						resourceName, tenantID, string(detailsJSON))
					if dbErr != nil {
						fmt.Printf("FEHLER beim Speichern des Alerts %s: %v\n", alertID, dbErr)
					}
				}

				if len(fetchedIDs) > 0 {
					db.Exec(`
						INSERT INTO acronis_alert_history (alert_id, cloud_name, type, severity, resource_name, tenant_id, created_at, resolved_at, details)
						SELECT id, cloud_name, type, severity, resource_name, tenant_id, created_at, NOW(), details
						FROM acronis_alerts
						WHERE cloud_name = $1 
						  AND id NOT IN (SELECT unnest($2::text[]))
						  AND id NOT IN (SELECT alert_id FROM acronis_alert_history WHERE resolved_at >= NOW() - INTERVAL '1 hour')
					`, cloud.Name, fetchedIDs)

					db.Exec(`
						DELETE FROM acronis_alerts 
						WHERE cloud_name = $1 
						  AND id NOT IN (
						      SELECT unnest($2::text[])
						  )
					`, cloud.Name, fetchedIDs)
				} else {
					db.Exec(`
						INSERT INTO acronis_alert_history (alert_id, cloud_name, type, severity, resource_name, tenant_id, created_at, resolved_at, details)
						SELECT id, cloud_name, type, severity, resource_name, tenant_id, created_at, NOW(), details
						FROM acronis_alerts
						WHERE cloud_name = $1
					`, cloud.Name)

					db.Exec(`DELETE FROM acronis_alerts WHERE cloud_name = $1`, cloud.Name)
				}
			}
		}

		fmt.Printf("Rufe Tasks für %s ab (Pagination läuft)...\n", cloud.Name)
		var allTasks []Task
		taskCursor := ""
		tasksFetchFailed := false

		for {
			q := url.Values{}
			q.Add("limit", "1000")
			// Nur Tasks der letzten 7 Tage holen, statt die komplette Historie zu paginieren
			q.Add("startedAt", "gt("+startOfWeekWindow.UTC().Format(time.RFC3339)+")")
			if taskCursor != "" {
				q.Add("after", taskCursor)
			}

			taskEndpoint := "/api/task_manager/v2/tasks?" + q.Encode()
			taskData, err := client.doRequest("GET", taskEndpoint)
			if err != nil {
				fmt.Printf("Fehler beim Abrufen der Tasks: %v\n", err)
				tasksFetchFailed = true
				break
			}

			if GetDebug {
				db.Exec("INSERT INTO acronis_raw_logs (cloud_name, endpoint, raw_response) VALUES ($1, $2, $3)",
					cloud.Name, "tasks", string(taskData))
			}

			var response struct {
				Items  []Task `json:"items"`
				Paging struct {
					Cursors struct {
						After string `json:"after"`
					} `json:"cursors"`
				} `json:"paging"`
			}

			if err := json.Unmarshal(taskData, &response); err != nil {
				fmt.Printf("FEHLER beim Parsen der Tasks: %v\n", err)
				tasksFetchFailed = true
				break
			}

			allTasks = append(allTasks, response.Items...)

			if response.Paging.Cursors.After == "" || len(response.Items) == 0 {
				break
			}
			taskCursor = response.Paging.Cursors.After
		}

		fmt.Printf("-> %d Tasks insgesamt für %s heruntergeladen.\n", len(allTasks), cloud.Name)

		weeklySuccessDays := make(map[string]map[string]bool)
		weeklyErrors := make(map[string]bool)
		dailySuccess := make(map[string]bool)
		dailyErrors := make(map[string]bool)
		yesterdayTasks := make(map[string][]diagTask)
		lastSuccess := make(map[string]time.Time)
		pastDays := make(map[string]*dayData)
		successTimes := make(map[string][]time.Time)

		taskSkippedZero := 0
		taskSkippedOld := 0
		taskInserted := 0
		yesterdayTypeStats := make(map[string]int)
		type unknownTaskInfo struct {
			ID           int64
			TaskType     string
			State        string
			Code         string
			ResourceID   string
			TaskResType  string
			CountsBackup bool
		}
		var yesterdayUnknown []unknownTaskInfo

		for _, task := range allTasks {
			taskTime := task.referenceTime()

			if taskTime.IsZero() {
				taskSkippedZero++
				continue
			}
			if taskTime.Before(startOfWeekWindow) {
				taskSkippedOld++
				continue
			}

			resourceID := ""
			if task.Resource != nil {
				resourceID = task.Resource.ID
			}
			if resourceID == "" {
				resourceID = task.ResourceID
			}
			resourceID = strings.ToLower(resourceID)

			name := machineNames[resourceID]
			if name == "" {
				if info, ok := allResources[resourceID]; ok && info.Name != "" {
					name = info.Name
				} else {
					name = "Unbekannt (Gelöscht/Fehlt)"
				}
			}

			resultJSON := string(task.Result)
			if resultJSON == "" {
				resultJSON = "null"
			}
			errorJSON, _ := json.Marshal(task.Error)

			_, dbErr := db.Exec("INSERT INTO acronis_tasks (id, cloud_name, resource_id, machine_name, state, result, error_details, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state, result = EXCLUDED.result, error_details = EXCLUDED.error_details",
				task.ID, cloud.Name, resourceID, name, task.State, resultJSON, string(errorJSON), taskTime)

			if dbErr != nil {
				fmt.Printf("FEHLER beim Speichern des Tasks %d: %v\n", task.ID, dbErr)
			} else {
				taskInserted++
			}

			isYesterday := !taskTime.Before(startOfYesterday) && taskTime.Before(startOfToday)
			code := task.resultCode()
			countsBackup := backupTaskTypes == nil || backupTaskTypes[strings.ToLower(task.Type)]

			// Laufende Tasks (enqueued/started/paused) haben noch kein Ergebnis
			completed := task.State == "completed"
			isError := (task.Error != nil && task.Error.Code != "") ||
				(completed && (code == "error" || code == "abandoned" || code == "timedout"))
			isSuccess := completed && !isError && (code == "ok" || code == "warning")

			dayStr := taskTime.In(time.Local).Format("2006-01-02")
			isPastDay := !taskTime.Before(startOfWeekWindow) && taskTime.Before(startOfYesterday)
			var pastDay *dayData
			if isPastDay && resourceID != "" {
				pastDay = pastDays[dayStr]
				if pastDay == nil {
					pastDay = newDayData()
					pastDays[dayStr] = pastDay
				}
			}

			if (isYesterday || pastDay != nil) && resourceID != "" {
				rawErr := task.rawError()
				dt := diagTask{
					ID:        task.ID,
					Type:      task.Type,
					Title:     task.contextTitle(),
					State:     task.State,
					Result:    code,
					Counted:   countsBackup,
					Error:     errorSummary(rawErr),
					RawError:  rawErr,
					refTime:   taskTime,
					isError:   isError,
					isSuccess: isSuccess,
				}
				if !task.StartedAt.IsZero() {
					dt.StartedAt = task.StartedAt.Time.In(time.Local).Format(time.RFC3339)
				}
				if !task.CompletedAt.IsZero() {
					dt.CompletedAt = task.CompletedAt.Time.In(time.Local).Format(time.RFC3339)
				}
				if isYesterday && len(yesterdayTasks[resourceID]) < 50 {
					yesterdayTasks[resourceID] = append(yesterdayTasks[resourceID], dt)
				} else if pastDay != nil && len(pastDay.tasks[resourceID]) < 50 {
					pastDay.tasks[resourceID] = append(pastDay.tasks[resourceID], dt)
				}
			}

			if isYesterday {
				yesterdayTypeStats[fmt.Sprintf("type=%s state=%s result=%s", task.Type, task.State, code)]++
				if resourceID != "" && machineNames[resourceID] == "" {
					taskResType := ""
					if task.Resource != nil {
						taskResType = task.Resource.Type
					}
					yesterdayUnknown = append(yesterdayUnknown, unknownTaskInfo{
						ID:           task.ID,
						TaskType:     task.Type,
						State:        task.State,
						Code:         code,
						ResourceID:   resourceID,
						TaskResType:  taskResType,
						CountsBackup: countsBackup,
					})
				}
			}

			// Nur Backup-Tasks bewerten, falls ACRONIS_BACKUP_TASK_TYPES gesetzt ist
			if !countsBackup {
				continue
			}
			if resourceID == "" {
				continue
			}

			if weeklySuccessDays[resourceID] == nil {
				weeklySuccessDays[resourceID] = make(map[string]bool)
			}

			if isSuccess && taskTime.After(lastSuccess[resourceID]) {
				lastSuccess[resourceID] = taskTime
			}
			if isSuccess {
				successTimes[resourceID] = append(successTimes[resourceID], taskTime)
			}
			if pastDay != nil {
				if isError {
					pastDay.errors[resourceID] = true
				} else if isSuccess {
					pastDay.success[resourceID] = true
				}
			}

			// Heutige Tasks werden gespeichert, zählen aber erst morgen in die Reports
			inWeekWindow := taskTime.Before(startOfToday)

			if isError {
				if inWeekWindow {
					weeklyErrors[resourceID] = true
				}
				if isYesterday {
					dailyErrors[resourceID] = true
				}
			} else if isSuccess {
				if inWeekWindow {
					dateStr := taskTime.In(time.Local).Format("2006-01-02")
					weeklySuccessDays[resourceID][dateStr] = true
				}
				if isYesterday {
					dailySuccess[resourceID] = true
				}
			}
		}

		fmt.Printf("-> Tasks Status: %d eingefügt, %d zu alt, %d leeres Datum.\n", taskInserted, taskSkippedOld, taskSkippedZero)

		// Gegenprüfung 2: Offline gemeldete Agents mit frischer Task-Aktivität gelten als online
		applyTaskActivity(agents, allTasks, machineNames, agentActivityWindow)

		agentInserted := 0
		for _, a := range agents {
			agentOnline[a.ID] = a.Online
			_, dbErr := db.Exec(`
				INSERT INTO acronis_agents (id, cloud_name, hostname, is_online, build_version, release_id, installer_version, online_source, last_seen_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
				ON CONFLICT (id) DO UPDATE SET
					cloud_name = EXCLUDED.cloud_name,
					hostname = EXCLUDED.hostname,
					is_online = EXCLUDED.is_online,
					build_version = EXCLUDED.build_version,
					release_id = EXCLUDED.release_id,
					installer_version = EXCLUDED.installer_version,
					online_source = EXCLUDED.online_source,
					last_seen_at = NOW()
			`, a.ID, cloud.Name, a.Hostname, a.Online, a.BuildVersion, a.ReleaseID, a.InstallerVersion, a.Source)
			if dbErr != nil {
				fmt.Printf("FEHLER beim Speichern des Agents %s: %v\n", a.ID, dbErr)
			} else {
				agentInserted++
			}
		}
		logAgentStatus(agents)
		fmt.Printf("-> %d Agents erfolgreich verarbeitet.\n", agentInserted)

		if len(yesterdayTypeStats) > 0 {
			fmt.Printf("-> Tasks von gestern nach Typ/Status/Ergebnis:\n")
			keys := make([]string, 0, len(yesterdayTypeStats))
			for k := range yesterdayTypeStats {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("   %5d  %s\n", yesterdayTypeStats[k], k)
			}
		} else {
			fmt.Printf("-> WARNUNG: Keine Tasks von gestern gefunden.\n")
		}
		if len(yesterdayUnknown) > 0 {
			fmt.Printf("-> WARNUNG: %d Tasks von gestern gehören zu keiner Report-Ressource:\n", len(yesterdayUnknown))
			for _, u := range yesterdayUnknown {
				where := "in Acronis nicht (mehr) vorhanden -> vermutlich gelöschte Ressource"
				if info, ok := allResources[u.ResourceID]; ok {
					where = fmt.Sprintf("Acronis-Ressource %q vom Typ %s -> nicht im Report-Filter", info.Name, info.Type)
				}
				backupHint := ""
				if !u.CountsBackup {
					backupHint = " [kein Backup-Typ laut ACRONIS_BACKUP_TASK_TYPES]"
				}
				fmt.Printf("   Task %d: type=%s state=%s result=%s resource=%s (%s)%s\n      %s\n",
					u.ID, u.TaskType, u.State, u.Code, u.ResourceID, u.TaskResType, backupHint, where)
			}
		}

		fmt.Printf("Rufe Backup-Pläne für %s ab...\n", cloud.Name)
		plans, planContexts, planPages, planErr := client.fetchBackupPlans()
		if GetDebug {
			for _, p := range planPages {
				db.Exec("INSERT INTO acronis_raw_logs (cloud_name, endpoint, raw_response) VALUES ($1, $2, $3)",
					cloud.Name, "policies", string(p))
			}
		}
		if planErr != nil {
			fmt.Printf("WARNUNG: Backup-Pläne konnten nicht vollständig abgerufen werden, Soll/Ist entfällt: %v\n", planErr)
		}
		schedules := buildSchedules(plans, planContexts, planErr, machineNames, allResources)
		fmt.Printf("-> %d Pläne mit Backup-Richtlinie gefunden.\n", len(plans))

		if tasksFetchFailed {
			// Unvollständige Daten würden sonst alle Maschinen auf false setzen
			fmt.Printf("-> Tasks unvollständig abgerufen, Wochen-/Tagesreports für %s werden in diesem Lauf NICHT aktualisiert.\n", cloud.Name)
		} else {
			reportUpserts := 0
			reportErrors := 0
			dailySuccessCount := 0
			planStats := make(map[int]int)
			var planGaps, planOther []string
			failureLog := make(map[string]string)

			for machineID, name := range machineNames {
				wDaysCount := len(weeklySuccessDays[machineID])
				wErrorFlag := weeklyErrors[machineID]

				// Soll/Ist laut Backup-Plan
				sched := schedules[machineID]
				expected := expectedBackupDates(sched, startOfWeekWindow, startOfToday, machineCreated[machineID])
				fulfilled := countFulfilledDays(expected, successTimes[machineID], scheduleGrace)
				planState, scheduleInfo := planUnknown, ""
				if sched != nil {
					planState = sched.State
					scheduleInfo = truncate(strings.Join(sched.Info, "; "), 500)
				}
				planStats[planState]++
				if planState == planScheduled && fulfilled < len(expected) {
					planGaps = append(planGaps, fmt.Sprintf("%s: Ist %d von Soll %d (%s)", name, fulfilled, len(expected), scheduleInfo))
				} else if planState != planScheduled {
					planOther = append(planOther, fmt.Sprintf("%s: %s", name, scheduleInfo))
				}

				// Wochenreport: genau eine Zeile pro Cloud + Maschine, wird bei jedem Lauf aktualisiert
				_, dbErr := db.Exec(`
					INSERT INTO acronis_weekly_reports (cloud_name, machine_id, machine_name, successful_backup_days, has_errors,
						expected_backup_days, fulfilled_backup_days, plan_state, schedule_info, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
					ON CONFLICT (cloud_name, machine_id) DO UPDATE SET
						machine_name = EXCLUDED.machine_name,
						successful_backup_days = EXCLUDED.successful_backup_days,
						has_errors = EXCLUDED.has_errors,
						expected_backup_days = EXCLUDED.expected_backup_days,
						fulfilled_backup_days = EXCLUDED.fulfilled_backup_days,
						plan_state = EXCLUDED.plan_state,
						schedule_info = EXCLUDED.schedule_info,
						updated_at = NOW()
				`, cloud.Name, machineID, name, wDaysCount, wErrorFlag, len(expected), fulfilled, planState, scheduleInfo)
				if dbErr != nil {
					fmt.Printf("FEHLER beim Speichern des Wochenreports für %s: %v\n", machineID, dbErr)
					reportErrors++
				} else {
					reportUpserts++
				}

				dSuccessFlag := dailySuccess[machineID]
				dErrorFlag := dailyErrors[machineID]
				if dSuccessFlag {
					dailySuccessCount++
				}

				details := yesterdayTasks[machineID]
				sort.Slice(details, func(i, j int) bool { return details[i].refTime.Before(details[j].refTime) })
				if details == nil {
					details = []diagTask{}
				}
				detailsJSON, _ := json.Marshal(details)

				failureReason := ""
				if !dSuccessFlag {
					online, known := agentOnline[machineID]
					failureReason = diagnoseBackup("Gestern", details, known, online, lastSuccess[machineID], startOfWeekWindow)
					failureLog[name] = failureReason
				}

				// Tagesreport: eine Zeile pro Cloud + Maschine + Datum.
				// Ein einmal festgestellter Erfolg/Fehler für den Tag wird von späteren Läufen nicht mehr zurückgesetzt.
				// Grund und Details werden aktualisiert, solange kein Erfolg vorliegt; findet ein späterer Lauf
				// keine Tasks mehr für den Tag, bleiben die zuletzt gespeicherten Details erhalten.
				_, dbErr = db.Exec(`
					INSERT INTO acronis_daily_reports (cloud_name, machine_id, machine_name, backup_successful, has_errors, report_date, failure_reason, backup_details, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, NOW())
					ON CONFLICT (cloud_name, machine_id, report_date) DO UPDATE SET
						machine_name = EXCLUDED.machine_name,
						backup_successful = acronis_daily_reports.backup_successful OR EXCLUDED.backup_successful,
						has_errors = acronis_daily_reports.has_errors OR EXCLUDED.has_errors,
						failure_reason = CASE
							WHEN acronis_daily_reports.backup_successful OR EXCLUDED.backup_successful THEN ''
							WHEN EXCLUDED.backup_details = '[]'::jsonb AND acronis_daily_reports.failure_reason <> '' THEN acronis_daily_reports.failure_reason
							ELSE EXCLUDED.failure_reason
						END,
						backup_details = CASE
							WHEN acronis_daily_reports.backup_successful AND NOT EXCLUDED.backup_successful THEN acronis_daily_reports.backup_details
							WHEN EXCLUDED.backup_details = '[]'::jsonb AND acronis_daily_reports.backup_details IS NOT NULL THEN acronis_daily_reports.backup_details
							ELSE EXCLUDED.backup_details
						END,
						updated_at = NOW()
				`, cloud.Name, machineID, name, dSuccessFlag, dErrorFlag, reportDateYesterdayStr, failureReason, string(detailsJSON))
				if dbErr != nil {
					fmt.Printf("FEHLER beim Speichern des Tagesreports für %s: %v\n", machineID, dbErr)
					reportErrors++
				} else {
					reportUpserts++
				}
			}

			fmt.Printf("-> Reports: %d gespeichert/aktualisiert, %d Fehler, %d von %d Maschinen mit erfolgreichem Backup gestern.\n",
				reportUpserts, reportErrors, dailySuccessCount, len(machineNames))

			fmt.Printf("-> Soll/Ist (7 Tage): %d Maschinen mit Zeitplan, %d ohne aktiven Plan, %d nicht auswertbar.\n",
				planStats[planScheduled], planStats[planNone], planStats[planUnknown])
			sort.Strings(planGaps)
			for _, g := range planGaps {
				fmt.Printf("   Unter Soll: %s\n", g)
			}
			sort.Strings(planOther)
			for _, o := range planOther {
				fmt.Printf("   Ohne Soll: %s\n", o)
			}

			if len(failureLog) > 0 {
				fmt.Printf("-> Gestern ohne erfolgreiches Backup (%d):\n", len(failureLog))
				names := make([]string, 0, len(failureLog))
				for n := range failureLog {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					fmt.Printf("   %s: %s\n", n, failureLog[n])
				}
			}

			if backfillDays > 0 {
				filled, filledDates, bfErr := backfillDailyReports(db, cloud.Name, machineNames, machineCreated, pastDays, successTimes,
					startOfWeekWindow, startOfYesterday, backfillDays)
				switch {
				case bfErr != nil:
					fmt.Printf("FEHLER beim Nachtragen fehlender Tagesreports: %v\n", bfErr)
				case filled > 0:
					fmt.Printf("-> %d fehlende Tagesreports nachgetragen (Tage: %s).\n", filled, strings.Join(filledDates, ", "))
				default:
					fmt.Printf("-> Keine fehlenden Tagesreports in den letzten %d Tagen.\n", backfillDays)
				}
			}
		}

		fmt.Printf("Rufe Activities für %s ab (Pagination läuft)...\n", cloud.Name)

		var allActivities []Activity
		cursor := ""
		pageCount := 0

		for {
			pageCount++
			qAct := url.Values{}
			qAct.Add("limit", "1000")
			if cursor != "" {
				qAct.Add("after", cursor)
			}

			actEndpoint := "/api/task_manager/v2/activities?" + qAct.Encode()
			actData, err := client.doRequest("GET", actEndpoint)
			if err != nil {
				fmt.Printf("Fehler beim Abrufen der Activities (Seite %d): %v\n", pageCount, err)
				break
			}

			var response struct {
				Items  []Activity `json:"items"`
				Paging struct {
					Cursors struct {
						After string `json:"after"`
					} `json:"cursors"`
				} `json:"paging"`
			}

			if err := json.Unmarshal(actData, &response); err != nil {
				fmt.Printf("FEHLER beim Parsen der Activities auf Seite %d: %v\n", pageCount, err)
				break
			}

			allActivities = append(allActivities, response.Items...)

			if response.Paging.Cursors.After == "" || len(response.Items) == 0 {
				break
			}
			cursor = response.Paging.Cursors.After
		}

		fmt.Printf("-> %d Activities insgesamt für %s heruntergeladen.\n", len(allActivities), cloud.Name)

		skippedZeroDate := 0
		skippedTooOld := 0
		inserted := 0

		for _, act := range allActivities {
			if act.CreatedAt.IsZero() {
				skippedZeroDate++
				continue
			}
			if act.CreatedAt.Before(sevenDaysAgo) {
				skippedTooOld++
				continue
			}

			state := act.State
			if state == "" {
				state = act.Status
			}

			var tID int64
			if act.TaskID != nil {
				tID = *act.TaskID
			}

			contextJSON, _ := json.Marshal(act.Context)
			_, dbErr := db.Exec("INSERT INTO acronis_activities (id, cloud_name, type, state, task_id, tenant_id, context, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state, context = EXCLUDED.context",
				act.ID, cloud.Name, act.Type, state, tID, act.TenantID, string(contextJSON), act.CreatedAt)

			if dbErr != nil {
				fmt.Printf("Fehler beim Speichern der Activity %v: %v\n", act.ID, dbErr)
			} else {
				inserted++
			}
		}

		fmt.Printf("-> %d in DB eingefügt, %d übersprungen (zu alt oder ungültiges Datum).\n", inserted, skippedTooOld+skippedZeroDate)
	}
}
