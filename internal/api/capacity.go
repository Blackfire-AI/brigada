package api

import (
	"net/http"
	"strconv"
)

// handleCapacity reads the capacity snapshot (§12.2): per agent the
// effective cap, the layer that sets it, the running jobs, the quota
// state and the cost weight; per layer — each runner, each account,
// each capped model, each active plan of the project and global — the
// cap, the use and the queue depth. That is what the foreman reads
// when it plans (§4.5). A foreman token is walled into its own
// project's plans (§12.1): an explicit ask for another project is
// refused, and no ask means its own. A client or admin may name any
// project, or none — 0 lists every active plan.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	project, err := queryInt64(w, r, "project")
	if err != nil {
		return
	}
	if id.role == roleForeman {
		if project != 0 && !id.mayTouchProject(project) {
			writeError(w, http.StatusForbidden, codeForbidden,
				"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
			return
		}
		project = id.project
	}
	snap, err := s.eng.Capacity(r.Context(), project)
	if err != nil {
		s.writeInternal(w, r, "read capacity", err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
