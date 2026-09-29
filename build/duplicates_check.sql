-- Zeigt Maschinen, die in den Reports unter mehreren machine_ids vorkommen (gleiche Cloud, gleicher Name).
-- Ändert nichts. Genau diese Gruppen führt der neue Collector beim ersten Lauf zusammen;
-- feste ID wird die mit dem ältesten Report (Spalte feste_id).
-- Ausführen:  psql "<DB_CONNECTION_STRING>" -f duplicates_check.sql

WITH ids AS (
	SELECT cloud_name,
	       LOWER(TRIM(MAX(machine_name))) AS name,
	       LOWER(machine_id) AS machine_id,
	       MIN(report_date::date) AS erster_report,
	       MAX(report_date::date) AS letzter_report,
	       MAX(report_date::date) FILTER (WHERE backup_successful) AS letztes_erfolgreiches
	FROM acronis_daily_reports
	WHERE machine_id IS NOT NULL AND machine_id <> ''
	GROUP BY cloud_name, LOWER(machine_id)
)
SELECT cloud_name,
       name AS machine_name,
       COUNT(*) AS anzahl_ids,
       (ARRAY_AGG(machine_id ORDER BY erster_report))[1] AS feste_id,
       STRING_AGG(machine_id || ' (' || erster_report || ' bis ' || letzter_report
                  || ', letztes OK: ' || COALESCE(letztes_erfolgreiches::text, 'nie') || ')',
                  E'\n' ORDER BY erster_report) AS ids
FROM ids
GROUP BY cloud_name, name
HAVING COUNT(*) > 1
ORDER BY cloud_name, name;
