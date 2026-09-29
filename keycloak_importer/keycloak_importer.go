package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

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

func main() {
	loadEnvFile(".env")

	listFlag := flag.Bool("list", false, "Listet alle Clients und deren Attribute aus Keycloak auf")
	flag.Parse()

	kcBaseURL := os.Getenv("KEYCLOAK_SERVER_URL")
	if kcBaseURL == "" {
		kcBaseURL = "https://k8s-keycloak.highq.org"
	}
	realm := os.Getenv("KEYCLOAK_REALM")
	if realm == "" {
		realm = "cloud_backups"
	}
	clientID := os.Getenv("KEYCLOAK_CLIENT_ID")
	if clientID == "" {
		clientID = "acronis-collector"
	}
	clientSecret := os.Getenv("KEYCLOAK_CLIENT_SECRET")

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", kcBaseURL, realm)
	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	reqToken, _ := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	reqToken.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqToken.SetBasicAuth(clientID, clientSecret)

	resp, err := httpClient.Do(reqToken)
	if err != nil {
		fmt.Printf("Verbindungsfehler zu Keycloak: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Authentifizierung fehlgeschlagen (HTTP Status: %d)\nAntwort: %s\n", resp.StatusCode, string(respBytes))
		os.Exit(1)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(respBytes, &tokenResp)

	if *listFlag {
		clientsURL := fmt.Sprintf("%s/admin/realms/%s/clients", kcBaseURL, realm)
		req, _ := http.NewRequest("GET", clientsURL, nil)
		req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)

		listResp, err := httpClient.Do(req)
		if err != nil {
			fmt.Printf("Fehler beim Abrufen der Clients: %v\n", err)
			os.Exit(1)
		}
		defer listResp.Body.Close()

		bodyBytes, _ := io.ReadAll(listResp.Body)

		if listResp.StatusCode != http.StatusOK {
			fmt.Printf("Fehler beim Laden der Clients (HTTP Status: %d)\nAntwort:\n%s\n", listResp.StatusCode, string(bodyBytes))
			os.Exit(1)
		}

		var clients []map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &clients); err != nil {
			fmt.Printf("Fehler beim Parsen der Client-Daten: %v\nRohes JSON:\n%s\n", err, string(bodyBytes))
			os.Exit(1)
		}

		fmt.Printf("\n--- Gefundene Keycloak Clients (%d) ---\n", len(clients))
		for _, c := range clients {
			cid, _ := c["clientId"].(string)
			fmt.Printf("- ClientId: %s\n", cid)
			if attrs, ok := c["attributes"].(map[string]interface{}); ok {
				for k, v := range attrs {
					if k == "base_url" || k == "username" || k == "password" {
						fmt.Printf("  -> %s: %v\n", k, v)
					}
				}
			}
		}
		return
	}

	clientBaseURL := os.Getenv("BASE_URL")
	if clientBaseURL == "" {
		clientBaseURL = "https://backup.ionos.com"
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("--- Keycloak Client Importer ---")
	fmt.Printf("Verwende Basis-URL aus Env für Attribute: %s\n\n", clientBaseURL)

	fmt.Print("ClientId eingeben (z.B. Kunde_hQMS): ")
	inputClientID, _ := reader.ReadString('\n')
	inputClientID = strings.TrimSpace(inputClientID)

	fmt.Print("Username eingeben: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)

	fmt.Print("Password eingeben: ")
	password, _ := reader.ReadString('\n')
	password = strings.TrimSpace(password)

	if inputClientID == "" || username == "" || password == "" {
		fmt.Println("Fehler: Alle Felder müssen ausgefüllt werden.")
		os.Exit(1)
	}

	clientPayload := map[string]interface{}{
		"clientId": inputClientID,
		"enabled":  true,
		"attributes": map[string]string{
			"base_url": clientBaseURL,
			"username": username,
			"password": password,
		},
	}

	payloadBytes, _ := json.Marshal(clientPayload)
	clientsURL := fmt.Sprintf("%s/admin/realms/%s/clients", kcBaseURL, realm)
	req, _ := http.NewRequest("POST", clientsURL, bytes.NewBuffer(payloadBytes))
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	createResp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("Fehler beim Senden des Requests: %v\n", err)
		os.Exit(1)
	}
	defer createResp.Body.Close()

	createBodyBytes, _ := io.ReadAll(createResp.Body)

	if createResp.StatusCode == http.StatusCreated || createResp.StatusCode == http.StatusNoContent {
		fmt.Printf("Erfolgreich: Client '%s' wurde in Keycloak angelegt.\n", inputClientID)
	} else {
		fmt.Printf("Fehler beim Erstellen des Clients (HTTP Status: %d)\n", createResp.StatusCode)
		if len(createBodyBytes) > 0 {
			fmt.Printf("Server-Antwort: %s\n", string(createBodyBytes))
		}
	}
}
