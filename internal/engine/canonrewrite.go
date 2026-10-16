package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CanonReport is what the startup identity rewrite did (design §13.1).
type CanonReport struct {
	// Rewritten counts the projects whose repo_url or transport changed.
	Rewritten int
	// Collisions are groups of project ids that canonicalize to the same
	// repository, by canonical form. Their rows are left untouched.
	Collisions map[string][]int64
	// Failed maps a project id to why its repo_url did not canonicalize
	// (the URL itself is never quoted). Those rows stay as they are.
	Failed map[int64]string
}

type projectIdentity struct {
	id        int64
	repo      string
	transport string
}

// CanonicalizeProjects rewrites every projects.repo_url to its canonical
// form in one transaction, and records the URL it was given as the
// project's transport where that is a usable Git URL (credentials
// dropped, never stored). Ids, plans, tasks, sessions and outcomes are
// untouched. Rows that canonicalize to the same value are not merged: the
// whole group is left as it is before anything is rewritten, and status
// lists it as needing the user. A row that fails to canonicalize stays as
// it is. Running it again changes nothing.
func (e *Engine) CanonicalizeProjects(ctx context.Context) (CanonReport, error) {
	rep := CanonReport{Collisions: map[string][]int64{}, Failed: map[int64]string{}}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		rep = CanonReport{Collisions: map[string][]int64{}, Failed: map[int64]string{}}
		rows, err := tx.Query("SELECT id, repo_url, transport_url FROM projects ORDER BY id")
		if err != nil {
			return fmt.Errorf("list the projects: %w", err)
		}
		var all []projectIdentity
		for rows.Next() {
			var p projectIdentity
			if err := rows.Scan(&p.id, &p.repo, &p.transport); err != nil {
				rows.Close()
				return fmt.Errorf("list the projects: %w", err)
			}
			all = append(all, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list the projects: %w", err)
		}
		rows.Close()

		parts := map[int64]repoParts{}
		// owners groups every row by the value its repo_url would hold:
		// the canonical form for a row that parses, the stored string for
		// one that does not, which still occupies it under the UNIQUE
		// constraint.
		owners := map[string][]int64{}
		for _, p := range all {
			rp, err := parseRepo(p.repo)
			if err != nil {
				rep.Failed[p.id] = err.Error()
				owners[p.repo] = append(owners[p.repo], p.id)
				continue
			}
			parts[p.id] = rp
			owners[rp.Canon] = append(owners[rp.Canon], p.id)
		}
		collided := map[int64]bool{}
		for canon, ids := range owners {
			if len(ids) < 2 {
				continue
			}
			rep.Collisions[canon] = ids
			for _, id := range ids {
				collided[id] = true
			}
		}
		for _, p := range all {
			rp, ok := parts[p.id]
			if !ok || collided[p.id] {
				continue
			}
			transport := p.transport
			if transport == "" {
				transport = rp.Transport
			}
			if p.repo == rp.Canon && transport == p.transport {
				continue
			}
			if _, err := tx.Exec("UPDATE projects SET repo_url = ?, transport_url = ? WHERE id = ?", rp.Canon, transport, p.id); err != nil {
				return fmt.Errorf("rewrite the identity of project %d: %w", p.id, err)
			}
			rep.Rewritten++
		}
		return nil
	})
	if err != nil {
		return CanonReport{}, fmt.Errorf("canonicalize the projects: %w", err)
	}
	return rep, nil
}

// duplicateProjectItemsTx lists the groups of projects that name one
// repository (design §13.1): the startup rewrite leaves them untouched,
// so the same grouping is read back here. project scopes the list to the
// groups holding it, 0 to all.
func duplicateProjectItemsTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	rows, err := tx.Query("SELECT id, repo_url FROM projects ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list the projects: %w", err)
	}
	defer rows.Close()
	groups := map[string][]int64{}
	for rows.Next() {
		var id int64
		var repo string
		if err := rows.Scan(&id, &repo); err != nil {
			return nil, fmt.Errorf("list the projects: %w", err)
		}
		canon, err := Canonicalize(repo)
		if err != nil {
			continue
		}
		groups[canon] = append(groups[canon], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the projects: %w", err)
	}
	var items []NeedsYouItem
	for canon, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		if project != 0 {
			in := false
			for _, id := range ids {
				in = in || id == project
			}
			if !in {
				continue
			}
		}
		items = append(items, NeedsYouItem{
			Kind:  "project",
			ID:    ids[0],
			State: "duplicate",
			Title: canon,
			Reason: fmt.Sprintf("projects %s are the same repository (%s); they were not merged: keep one and retire the other, or tell the foreman which one is right",
				joinProjectIDs(ids), canon),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

// joinProjectIDs renders ids as "1 and 2" or "1, 2 and 3".
func joinProjectIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	if len(s) == 1 {
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}
