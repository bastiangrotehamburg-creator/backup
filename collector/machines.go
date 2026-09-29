package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// acronis_machines hält pro Cloud eine feste Identität je Maschine.
// machine_id ist die erste bekannte Acronis-ID und ändert sich nie mehr, damit Zabbix die Maschine
// nicht als neue (und die alte als fehlerhafte) Maschine sieht. current_id ist die aktuelle Acronis-ID;
// frühere IDs landen mit Zeitpunkt in old_machine_ids.
const machinesMigrationSQL = `CREATE TABLE IF NOT EXISTS acronis_machines (
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
)`

func ensureMachinesTable(db *sql.DB) error {
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass('acronis_machines') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(machinesMigrationSQL); err != nil {
		return err
	}
	fmt.Println("Tabelle acronis_machines angelegt (feste machine_id je Maschine)")
	return nil
}

type oldMachineID struct {
	MachineID  string `json:"machine_id"`
	ReplacedAt string `json:"replaced_at"`
}

type machineRow struct {
	MachineID    string
	MachineName  string
	CurrentID    string
	ResourceType string
	TenantID     string
	OldIDs       []oldMachineID
}

func (r *machineRow) hasID(id string) bool {
	if r.MachineID == id || r.CurrentID == id {
		return true
	}
	for _, o := range r.OldIDs {
		if o.MachineID == id {
			return true
		}
	}
	return false
}

func (r *machineRow) addOld(id, replacedAt string) {
	if id == "" || id == r.CurrentID {
		return
	}
	for _, o := range r.OldIDs {
		if o.MachineID == id {
			return
		}
	}
	r.OldIDs = append(r.OldIDs, oldMachineID{MachineID: id, ReplacedAt: replacedAt})
}

// machineIdentities ordnet aktuelle Acronis-IDs der festen machine_id zu.
type machineIdentities struct {
	stable map[string]string
	// superseded: doppelte Acronis-Ressourcen derselben Maschine (ältere Registrierung) -> aktuelle ID.
	// Für sie werden keine Reports mehr geschrieben.
	superseded map[string]string
}

func (m machineIdentities) id(acronisID string) string {
	if s, ok := m.stable[acronisID]; ok {
		return s
	}
	return acronisID
}

func (m machineIdentities) skip(acronisID string) bool {
	_, ok := m.superseded[acronisID]
	return ok
}

func identityFallback() machineIdentities {
	return machineIdentities{stable: map[string]string{}, superseded: map[string]string{}}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func compatible(a, b string) bool {
	return a == "" || b == "" || strings.EqualFold(a, b)
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		fmt.Printf("WARNUNG: %s=%q ist kein gültiger Wahrheitswert, verwende %v\n", key, v, def)
	}
	return def
}

// resolveMachineIdentities gleicht die Report-Ressourcen mit acronis_machines ab.
// Eine Maschine wird über Name + Typ (+ Tenant, falls bekannt) wiedererkannt, wenn ihre bisherige
// Acronis-ID verschwunden ist oder (MERGE_DUPLICATE_MACHINES, Standard an) parallel eine neuere
// Registrierung mit gleichem Namen existiert. Reports der alten IDs werden unter die feste ID verschoben.
func resolveMachineIdentities(db *sql.DB, cloudName string, items []ResourceItem) (machineIdentities, error) {
	ids := identityFallback()
	mergeDuplicates := envBool("MERGE_DUPLICATE_MACHINES", true)
	now := time.Now().Format(time.RFC3339)

	var rows []*machineRow
	dbRows, err := db.Query(`
		SELECT machine_id, machine_name, current_id, resource_type, tenant_id, old_machine_ids::text
		FROM acronis_machines WHERE cloud_name = $1 ORDER BY created_at, machine_id`, cloudName)
	if err != nil {
		return ids, err
	}
	for dbRows.Next() {
		r := &machineRow{}
		var old string
		if err := dbRows.Scan(&r.MachineID, &r.MachineName, &r.CurrentID, &r.ResourceType, &r.TenantID, &old); err != nil {
			dbRows.Close()
			return ids, err
		}
		json.Unmarshal([]byte(old), &r.OldIDs)
		rows = append(rows, r)
	}
	dbRows.Close()
	if err := dbRows.Err(); err != nil {
		return ids, err
	}

	// Bisherige Report-IDs mit erstem Reportdatum, um beim ersten Lauf bestehende Duplikate zusammenzuführen
	type histID struct{ id, first string }
	histByName := make(map[string][]histID)
	firstReport := make(map[string]string)
	hRows, err := db.Query(`
		SELECT LOWER(machine_id), LOWER(MAX(machine_name)), MIN(report_date::date)::text
		FROM acronis_daily_reports
		WHERE cloud_name = $1 AND machine_id IS NOT NULL AND machine_id <> ''
		GROUP BY LOWER(machine_id)`, cloudName)
	if err != nil {
		return ids, err
	}
	for hRows.Next() {
		var id, name, first string
		if hRows.Scan(&id, &name, &first) == nil {
			histByName[name] = append(histByName[name], histID{id, first})
			firstReport[id] = first
		}
	}
	hRows.Close()

	// Report-Ressourcen nach Maschine gruppieren: gleicher Name, Typ und Tenant
	present := make(map[string]bool)
	groups := make(map[string][]ResourceItem)
	namesPerName := make(map[string]map[string]bool)
	for _, it := range items {
		id := strings.ToLower(it.ID)
		if id == "" {
			continue
		}
		present[id] = true
		name := strings.ToLower(strings.TrimSpace(it.Name))
		key := name + "|" + strings.ToLower(it.Type) + "|" + strings.ToLower(it.TenantID)
		if !mergeDuplicates || name == "" {
			key = "id|" + id
		}
		groups[key] = append(groups[key], it)
		if namesPerName[name] == nil {
			namesPerName[name] = make(map[string]bool)
		}
		namesPerName[name][key] = true
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var changes []string
	assigned := make(map[*machineRow]bool)
	for _, key := range keys {
		g := groups[key]
		// Neueste Registrierung zuerst: sie ist die aktuelle
		sort.SliceStable(g, func(i, j int) bool { return g[i].CreatedAt.Time.After(g[j].CreatedAt.Time) })
		gIDs := make([]string, len(g))
		for i, it := range g {
			gIDs[i] = strings.ToLower(it.ID)
		}
		name := g[0].Name
		lname := strings.ToLower(strings.TrimSpace(name))

		// Passende Einträge: zuerst über eine ID, sonst über den Namen, wenn die bisherige ID verschwunden ist
		// Ein Eintrag, dessen aktuelle ID noch in einer anderen Gruppe existiert, gehört zu dieser.
		var matches []*machineRow
		for _, r := range rows {
			if assigned[r] || (present[r.CurrentID] && !contains(gIDs, r.CurrentID)) {
				continue
			}
			for _, id := range gIDs {
				if r.hasID(id) {
					matches = append(matches, r)
					break
				}
			}
		}
		if len(matches) == 0 && lname != "" && mergeDuplicates {
			for _, r := range rows {
				if !assigned[r] && strings.ToLower(strings.TrimSpace(r.MachineName)) == lname && !present[r.CurrentID] &&
					compatible(r.ResourceType, g[0].Type) && compatible(r.TenantID, g[0].TenantID) {
					matches = append(matches, r)
				}
			}
		}

		current := gIDs[0]
		isNew := len(matches) == 0
		var row *machineRow
		if isNew {
			// Erster Lauf für diese Maschine: auch verschwundene IDs mit gleichem Namen aus den Reports übernehmen.
			// Bei mehreren Maschinen gleichen Namens (z. B. VM und Agent) ist das nicht eindeutig und unterbleibt.
			candidates := append([]string(nil), gIDs...)
			if mergeDuplicates && lname != "" && len(namesPerName[lname]) == 1 {
				for _, h := range histByName[lname] {
					if present[h.id] {
						continue
					}
					claimed := false
					for _, r := range rows {
						if r.hasID(h.id) {
							claimed = true
							break
						}
					}
					if !claimed {
						candidates = append(candidates, h.id)
					}
				}
			}
			// Feste ID: die mit dem ältesten Report (daran hängen die Zabbix-Items), sonst die älteste Registrierung
			stable := gIDs[len(gIDs)-1]
			bestFirst := ""
			for _, c := range candidates {
				if f, ok := firstReport[c]; ok && (bestFirst == "" || f < bestFirst) {
					stable, bestFirst = c, f
				}
			}
			row = &machineRow{MachineID: stable, CurrentID: current}
			for _, c := range candidates {
				row.addOld(c, now)
			}
			rows = append(rows, row)
		} else {
			row = matches[0]
			// Weitere Einträge derselben Maschine einverleiben
			for _, x := range matches[1:] {
				row.addOld(x.MachineID, now)
				row.addOld(x.CurrentID, now)
				for _, o := range x.OldIDs {
					row.addOld(o.MachineID, o.ReplacedAt)
				}
				if err := mergeMachineRow(db, cloudName, x.MachineID, row.MachineID); err != nil {
					return ids, err
				}
				changes = append(changes, fmt.Sprintf("%s: Eintrag %s mit %s zusammengeführt", name, x.MachineID, row.MachineID))
				for i, r := range rows {
					if r == x {
						rows = append(rows[:i], rows[i+1:]...)
						break
					}
				}
			}
			if present[row.CurrentID] && row.CurrentID != current && g[0].CreatedAt.IsZero() {
				// Ohne Anlagedatum die bisherige aktuelle ID behalten
				current = row.CurrentID
			}
			if row.CurrentID != current {
				changes = append(changes, fmt.Sprintf("%s: neue Acronis-ID %s (bisher %s, feste ID %s)", name, current, row.CurrentID, row.MachineID))
				prev := row.CurrentID
				row.CurrentID = current
				row.addOld(prev, now)
			}
			for _, id := range gIDs {
				row.addOld(id, now)
			}
		}
		// Eine wieder aufgetauchte ID ist nicht mehr "alt"
		kept := row.OldIDs[:0]
		for _, o := range row.OldIDs {
			if o.MachineID != row.CurrentID {
				kept = append(kept, o)
			}
		}
		row.OldIDs = kept
		if row.OldIDs == nil {
			row.OldIDs = []oldMachineID{}
		}
		assigned[row] = true
		row.MachineName = name
		row.ResourceType = g[0].Type
		if g[0].TenantID != "" {
			row.TenantID = g[0].TenantID
		}

		if isNew && len(row.OldIDs) > 0 {
			changes = append(changes, fmt.Sprintf("%s: %d IDs zusammengeführt, feste ID %s, aktuell %s", name, len(row.OldIDs)+1, row.MachineID, row.CurrentID))
		}

		oldJSON, _ := json.Marshal(row.OldIDs)
		if _, err := db.Exec(`
			INSERT INTO acronis_machines (cloud_name, machine_id, machine_name, current_id, resource_type, tenant_id, old_machine_ids, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, NOW())
			ON CONFLICT (cloud_name, machine_id) DO UPDATE SET
				machine_name = EXCLUDED.machine_name,
				current_id = EXCLUDED.current_id,
				resource_type = EXCLUDED.resource_type,
				tenant_id = EXCLUDED.tenant_id,
				old_machine_ids = EXCLUDED.old_machine_ids,
				updated_at = NOW()
		`, cloudName, row.MachineID, row.MachineName, row.CurrentID, row.ResourceType, row.TenantID, string(oldJSON)); err != nil {
			return ids, fmt.Errorf("Maschine %s: %w", name, err)
		}

		// Reports früherer IDs unter die feste ID verschieben (No-op, wenn schon geschehen)
		for _, o := range row.OldIDs {
			if o.MachineID != row.MachineID {
				if err := mergeMachineReports(db, cloudName, o.MachineID, row.MachineID); err != nil {
					return ids, err
				}
			}
		}
		if row.CurrentID != row.MachineID {
			if err := mergeMachineReports(db, cloudName, row.CurrentID, row.MachineID); err != nil {
				return ids, err
			}
		}

		for _, id := range gIDs {
			ids.stable[id] = row.MachineID
			if id != row.CurrentID {
				ids.superseded[id] = row.CurrentID
			}
		}
	}

	for _, c := range changes {
		fmt.Printf("-> Maschinen-ID: %s\n", c)
	}
	for id, cur := range ids.superseded {
		fmt.Printf("-> WARNUNG: Acronis-Ressource %s ist eine ältere Registrierung von %s (gleicher Name) und wird nicht mehr ausgewertet. "+
			"Bitte in Acronis entfernen oder MERGE_DUPLICATE_MACHINES=false setzen.\n", id, cur)
	}
	return ids, nil
}

// mergeMachineRow entfernt einen doppelten acronis_machines-Eintrag und verschiebt seine Reports.
func mergeMachineRow(db *sql.DB, cloudName, from, to string) error {
	if _, err := db.Exec(`DELETE FROM acronis_machines WHERE cloud_name = $1 AND machine_id = $2`, cloudName, from); err != nil {
		return err
	}
	return mergeMachineReports(db, cloudName, from, to)
}

// mergeMachineReports verschiebt Tages- und Wochenreports von einer alten Acronis-ID auf die feste ID.
// Gibt es für einen Tag schon einen Report der festen ID, gewinnt ein erfolgreiches Backup.
func mergeMachineReports(db *sql.DB, cloudName, from, to string) error {
	if from == "" || from == to {
		return nil
	}
	var n int
	if err := db.QueryRow(`
		SELECT (SELECT COUNT(*) FROM acronis_daily_reports WHERE cloud_name = $1 AND LOWER(machine_id) = $2)
		     + (SELECT COUNT(*) FROM acronis_weekly_reports WHERE cloud_name = $1 AND LOWER(machine_id) = $2)
	`, cloudName, from).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`UPDATE acronis_daily_reports s SET
			backup_successful = s.backup_successful OR o.backup_successful,
			has_errors = s.has_errors OR o.has_errors,
			failure_reason = CASE WHEN s.backup_successful OR o.backup_successful THEN '' ELSE s.failure_reason END,
			backup_details = CASE WHEN NOT s.backup_successful AND o.backup_successful THEN o.backup_details ELSE s.backup_details END,
			updated_at = NOW()
		FROM acronis_daily_reports o
		WHERE s.cloud_name = $1 AND s.machine_id = $3
		  AND o.cloud_name = $1 AND LOWER(o.machine_id) = $2 AND o.report_date = s.report_date`,
		`DELETE FROM acronis_daily_reports o USING acronis_daily_reports s
		WHERE o.cloud_name = $1 AND LOWER(o.machine_id) = $2
		  AND s.cloud_name = $1 AND s.machine_id = $3 AND s.report_date = o.report_date`,
		`UPDATE acronis_daily_reports SET machine_id = $3 WHERE cloud_name = $1 AND LOWER(machine_id) = $2`,
		// Wochenreport wird im selben Lauf für die feste ID neu berechnet
		`DELETE FROM acronis_weekly_reports o
		WHERE o.cloud_name = $1 AND LOWER(o.machine_id) = $2
		  AND EXISTS (SELECT 1 FROM acronis_weekly_reports s WHERE s.cloud_name = $1 AND s.machine_id = $3)`,
		`UPDATE acronis_weekly_reports SET machine_id = $3 WHERE cloud_name = $1 AND LOWER(machine_id) = $2`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q, cloudName, from, to); err != nil {
			return fmt.Errorf("Reports von %s nach %s verschieben: %w", from, to, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("-> Reports von %s auf feste ID %s übertragen (%d Zeilen)\n", from, to, n)
	return nil
}
