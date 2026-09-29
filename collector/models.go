package main

import (
	"time"
)

type AuthResponse struct {
	AccessToken string `json:"access_token"`
}

type ActivityList struct {
	Items []Activity `json:"items"`
}

type TaskList struct {
	Items []Task `json:"items"`
}

type TaskPolicy struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type TaskProgress struct {
	Current int `json:"current,omitempty"`
	Total   int `json:"total,omitempty"`
}

type TaskResource struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type TaskError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Domain  string `json:"domain,omitempty"`
}

type AgentVersionDetail struct {
	ReleaseID string `json:"release_id,omitempty"`
	Build     string `json:"build,omitempty"`
}

type AgentCoreVersion struct {
	Current *AgentVersionDetail `json:"current,omitempty"`
}

type Agent struct {
	ID          string            `json:"id"`
	Hostname    string            `json:"hostname,omitempty"`
	Online      *bool             `json:"online,omitempty"`
	CoreVersion *AgentCoreVersion `json:"core_version,omitempty"`
}

type AgentList struct {
	Items []Agent `json:"items"`
}

type AlertList struct {
	Items []struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		Severity  string    `json:"severity"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"items"`
}
