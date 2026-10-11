package config

import (
	"fmt"
	"strings"
)

// FieldGuard names a field in a project that a connection type with write
// tools treats specially — youtrack-rest reads protected_fields (never
// written) and set_once_fields (written only while empty) into these.
// Project "" means every project. Matching is case-insensitive.
type FieldGuard struct {
	Project string
	Field   string
}

// ParseFieldGuards reads "Project:Field" and "Field" entries. "*" stands for
// every project. Field names keep their spelling; an entry without a field
// name is an error, so a typo like "P:" is caught where the list is entered
// (desktop form, connection_add) instead of at the next registration.
func ParseFieldGuards(entries []string) ([]FieldGuard, error) {
	var out []FieldGuard
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		project, field, found := strings.Cut(e, ":")
		if !found {
			field, project = project, ""
		}
		project = strings.TrimSpace(project)
		field = strings.TrimSpace(field)
		if project == "*" {
			project = ""
		}
		if field == "" {
			return nil, fmt.Errorf("entry %q names no field (expected \"Project:Field\" or \"Field\")", e)
		}
		out = append(out, FieldGuard{Project: project, Field: field})
	}
	return out, nil
}

// ValidateFieldGuards is ParseFieldGuards for callers that only need the
// verdict.
func ValidateFieldGuards(entries []string) error {
	_, err := ParseFieldGuards(entries)
	return err
}

// MatchesProject reports whether the guard applies in a project.
func (g FieldGuard) MatchesProject(project string) bool {
	return g.Project == "" || strings.EqualFold(g.Project, project)
}

// Matches reports whether the guard names this field in this project.
func (g FieldGuard) Matches(project, field string) bool {
	return g.MatchesProject(project) && strings.EqualFold(g.Field, field)
}
