package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// Volumes live on the node that first ran a replica; the replica is pinned there.
const volumesSchema = `
CREATE TABLE IF NOT EXISTS volume_pins (project TEXT NOT NULL, grp TEXT NOT NULL, replica INTEGER NOT NULL,
  node_id TEXT NOT NULL, volumes TEXT NOT NULL DEFAULT '[]', created INTEGER NOT NULL, PRIMARY KEY(project, grp, replica));`

func (s *Server) initVolumes() error {
	_, err := s.st.DB.Exec(volumesSchema)
	return err
}

// volumePin returns the node holding a group replica's volumes ("" if none yet).
func (s *Server) volumePin(project, group string, replica int) string {
	var node string
	_ = s.st.DB.QueryRow(`SELECT node_id FROM volume_pins WHERE project=? AND grp=? AND replica=?`, project, group, replica).Scan(&node)
	return node
}

// pinVolume records where a replica's volumes live; new volume names are merged in.
func (s *Server) pinVolume(project, group string, replica int, node string, names []string) {
	var cur string
	have := map[string]bool{}
	var list []string
	if s.st.DB.QueryRow(`SELECT volumes FROM volume_pins WHERE project=? AND grp=? AND replica=?`, project, group, replica).Scan(&cur) == nil {
		_ = json.Unmarshal([]byte(cur), &list)
		for _, n := range list {
			have[n] = true
		}
	}
	for _, n := range names {
		if !have[n] {
			list = append(list, n)
		}
	}
	_, _ = s.st.DB.Exec(`INSERT INTO volume_pins(project,grp,replica,node_id,volumes,created) VALUES(?,?,?,?,?,?)
		ON CONFLICT(project,grp,replica) DO UPDATE SET volumes=excluded.volumes`, project, group, replica, node, store.JSON(list), store.Now())
}

// VolumeInfo describes a pinned replica volume set.
type VolumeInfo struct {
	Group    string   `json:"group"`
	Replica  int      `json:"replica"`
	NodeID   string   `json:"node_id"`
	NodeName string   `json:"node_name,omitempty"`
	Volumes  []string `json:"volumes"`
	InUse    bool     `json:"in_use"`
}

func (s *Server) registerVolumeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{project}/volumes", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		writeJSON(w, http.StatusOK, api.List[VolumeInfo]{Items: s.volumeInfos(p, "")})
	}))
	// Deleting forgets the pin and asks the node to remove the data. Refused while in use.
	mux.HandleFunc("DELETE /v1/projects/{project}/volumes/{group}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		group := r.PathValue("group")
		only := -1
		if v := r.URL.Query().Get("replica"); v != "" {
			only, _ = strconv.Atoi(v)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var todo []VolumeInfo
		for _, vi := range s.volumeInfos(p, group) {
			if only >= 0 && vi.Replica != only {
				continue
			}
			if vi.InUse {
				problem(w, http.StatusConflict, "volume in use", fmt.Sprintf("replica %d of %s is running; stop or scale down the group first", vi.Replica, group))
				return
			}
			if s.hub.get(vi.NodeID) == nil {
				problem(w, http.StatusConflict, "node offline", fmt.Sprintf("node %s holds replica %d's volumes and is offline; retry when it is back", orName(vi.NodeName, vi.NodeID), vi.Replica))
				return
			}
			todo = append(todo, vi)
		}
		removed := 0
		for _, vi := range todo {
			if !s.hub.send(vi.NodeID, proto.TypeVolumeDelete, proto.VolumeDelete{Names: vi.Volumes}) {
				continue
			}
			_, _ = s.st.DB.Exec(`DELETE FROM volume_pins WHERE project=? AND grp=? AND replica=?`, p, group, vi.Replica)
			removed++
		}
		if removed == 0 {
			problem(w, http.StatusNotFound, "not found", "no volumes for that group/replica")
			return
		}
		s.st.AddEvent(p, "", "", "", "volume.deleted", "Volumes of "+group+" deleted by "+u.Name)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func orName(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

func (s *Server) volumeInfos(project, group string) []VolumeInfo {
	q := `SELECT grp, replica, node_id, volumes FROM volume_pins WHERE project=?`
	args := []any{project}
	if group != "" {
		q += ` AND grp=?`
		args = append(args, group)
	}
	rows, err := s.st.DB.Query(q+` ORDER BY grp, replica`, args...)
	if err != nil {
		return nil
	}
	var out []VolumeInfo
	for rows.Next() {
		var vi VolumeInfo
		var vols string
		if rows.Scan(&vi.Group, &vi.Replica, &vi.NodeID, &vols) == nil {
			_ = json.Unmarshal([]byte(vols), &vi.Volumes)
			out = append(out, vi)
		}
	}
	rows.Close()
	for i := range out {
		vi := &out[i]
		if n, err := s.st.NodeByID(vi.NodeID); err == nil {
			vi.NodeName = n.Name
		}
		if g, err := s.st.Group(project, vi.Group); err == nil {
			var n int
			_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM instances WHERE group_id=? AND replica_index=? AND state!='terminated'`, g.ID, vi.Replica).Scan(&n)
			vi.InUse = n > 0
		}
		if vi.Volumes == nil {
			vi.Volumes = []string{}
		}
	}
	if out == nil {
		out = []VolumeInfo{}
	}
	return out
}
