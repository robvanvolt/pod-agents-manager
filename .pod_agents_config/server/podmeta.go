package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type podMeta struct {
	Notes    string   `json:"notes"`
	Tags     []string `json:"tags"`
	Favorite bool     `json:"favorite"`
}

func podMetaPath(agent, instance string) string {
	return filepath.Join(os.Getenv("HOME"), "Developer", agent+"-pods", instance, ".pod_meta.json")
}

func loadPodMeta(agent, instance string) podMeta {
	m := podMeta{Tags: []string{}}
	path := podMetaPath(agent, instance)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return m
}

func savePodMeta(agent, instance string, m podMeta) error {
	path := podMetaPath(agent, instance)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
