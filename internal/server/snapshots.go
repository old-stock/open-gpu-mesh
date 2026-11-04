package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// Volume snapshots (EBS-snapshot-like): the node holding a replica's volumes streams them
// as a .tar.gz into the hub file store; a snapshot can be restored onto the same or another
// node (which also re-pins the replica there: that is how a volume migrates between nodes).

const snapshotsSchema = `CREATE TABLE IF NOT EXISTS volume_snapshots(
  id TEXT PRIMARY KEY, project TEXT NOT NULL, grp TEXT NOT NULL, replica INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '',
  volumes TEXT NOT NULL, node_id TEXT NOT NULL, status TEXT NOT NULL, bytes INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
  restore_status TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0);`

// Snapshot is the API representation.
type Snapshot struct {
	ID            string     `json:"id"`
	Name          string     `json:"name,omitempty"`
	Group         string     `json:"group"`
	Replica       int        `json:"replica"`
	Volumes       []string   `json:"volumes"`
	NodeID        string     `json:"node_id"`
	Status        string     `json:"status"` // pending | ready | failed
	Bytes         int64      `json:"bytes"`
	Error         string     `json:"error,omitempty"`
	RestoreStatus string     `json:"restore_status,omitempty"` // "", restoring, restored, failed: ...
	CreateTime    time.Time  `json:"create_time"`
	FinishTime    *time.Time `json:"finish_time,omitempty"`
}

type pendingRestore struct {
	project, group string
	replica        int
	node           string
	names          []string
}

var restoreMu sync.Mutex
var restores = map[string]pendingRestore{} // op id -> target

func (s *Server) initSnapshots() error {
	_, err := s.st.DB.Exec(snapshotsSchema)
	return err
}

func snapshotPath(group, id string) string { return ".snapshots/" + group + "/" + id + ".tar.gz" }

func (s *Server) snapshots(where string, args ...any) []Snapshot {
	rows, err := s.st.DB.Query(`SELECT id,name,grp,replica,volumes,node_id,status,bytes,error,restore_status,created,finished FROM volume_snapshots `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var sn Snapshot
		var vols string
		var created, fin int64
		if rows.Scan(&sn.ID, &sn.Name, &sn.Group, &sn.Replica, &vols, &sn.NodeID, &sn.Status, &sn.Bytes, &sn.Error, &sn.RestoreStatus, &created, &fin) != nil {
			continue
		}
		var vms []proto.VolumeMount
		_ = json.Unmarshal([]byte(vols), &vms)
		sn.Volumes = []string{}
		for _, v := range vms {
			sn.Volumes = append(sn.Volumes, v.Name)
		}
		sn.CreateTime = store.T(created)
		if fin > 0 {
			t := store.T(fin)
			sn.FinishTime = &t
		}
		out = append(out, sn)
	}
	return out
}

// onVolumeOpDone handles the agent's report.
func (s *Server) onVolumeOpDone(ns *nodeSession, d proto.VolumeOpDone) {
	if d.Op == "snapshot" {
		status := "ready"
		if d.Error != "" {
			status = "failed"
		}
		var project, group string
		_ = s.st.DB.QueryRow(`SELECT project, grp FROM volume_snapshots WHERE id=? AND node_id=?`, d.ID, ns.nodeID).Scan(&project, &group)
		if project == "" {
			return
		}
		_, _ = s.st.DB.Exec(`UPDATE volume_snapshots SET status=?, bytes=?, error=?, finished=? WHERE id=?`, status, d.Bytes, d.Error, store.Now(), d.ID)
		if status == "ready" {
			_, _ = s.st.DB.Exec(`INSERT INTO files(project,path,size,sha256,content_type,created) VALUES(?,?,?,?,?,?)
				ON CONFLICT(project,path) DO NOTHING`, project, snapshotPath(group, d.ID), d.Bytes, "", "application/gzip", store.Now())
		}
		s.st.AddEvent(project, "", "", ns.nodeID, "volume.snapshot", fmt.Sprintf("Snapshot %s of %s: %s %s", d.ID, group, status, d.Error))
		return
	}
	// restore: op id is "<snapshot id>:<nonce>"
	restoreMu.Lock()
	pr, ok := restores[d.ID]
	delete(restores, d.ID)
	restoreMu.Unlock()
	if !ok || pr.node != ns.nodeID {
		return
	}
	snapID := d.ID
	if i := len(snapID) - 9; i > 0 && snapID[i] == ':' {
		snapID = snapID[:i]
	}
	status := "restored"
	if d.Error != "" {
		status = "failed: " + d.Error
	} else {
		_, _ = s.st.DB.Exec(`DELETE FROM volume_pins WHERE project=? AND grp=? AND replica=?`, pr.project, pr.group, pr.replica)
		s.pinVolume(pr.project, pr.group, pr.replica, pr.node, pr.names)
		s.Poke()
	}
	_, _ = s.st.DB.Exec(`UPDATE volume_snapshots SET restore_status=? WHERE id=?`, status, snapID)
	s.st.AddEvent(pr.project, "", "", ns.nodeID, "volume.restore", fmt.Sprintf("Restore of snapshot %s into %s replica %d: %s", snapID, pr.group, pr.replica, status))
}

func (s *Server) registerSnapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{project}/volumes/{group}/snapshots", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var body struct {
			Replica int    `json:"replica"`
			Name    string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		group := r.PathValue("group")
		var vi *VolumeInfo
		for _, v := range s.volumeInfos(p, group) {
			if v.Replica == body.Replica {
				vi = &v
				break
			}
		}
		if vi == nil {
			problem(w, http.StatusNotFound, "not found", fmt.Sprintf("no volumes for %s replica %d", group, body.Replica))
			return
		}
		g, err := s.st.Group(p, group)
		if err != nil {
			problem(w, http.StatusConflict, "group deleted", "snapshots need the group definition (its image is used to read the volume); redeploy it stopped first")
			return
		}
		if s.hub.get(vi.NodeID) == nil {
			problem(w, http.StatusConflict, "node offline", "node "+orName(vi.NodeName, vi.NodeID)+" holds the volumes and is offline")
			return
		}
		var vms []proto.VolumeMount
		for _, v := range g.Spec.Container.Volumes {
			vms = append(vms, proto.VolumeMount{Name: v.Name, Source: volumeName(p, group, v.Name, body.Replica), Target: v.Mount})
		}
		id := store.NewID("snap")
		vb, _ := json.Marshal(vms)
		now := store.Now()
		if _, err := s.st.DB.Exec(`INSERT INTO volume_snapshots(id,project,grp,replica,name,volumes,node_id,status,created) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, p, group, body.Replica, body.Name, string(vb), vi.NodeID, "pending", now); err != nil {
			storeErr(w, err, "snapshot")
			return
		}
		fp := snapshotPath(group, id)
		exp := time.Now().Add(6 * time.Hour).Unix()
		rel := fmt.Sprintf("/v1/signed/%s/%s?method=PUT&exp=%d&sig=%s", p, fp, exp, s.signFile("PUT", p, fp, exp))
		auth := g.Spec.Container.RegistryAuth
		if auth != nil {
			cp := *auth
			cp.Password, _ = s.expandSecrets(p, cp.Password)
			auth = &cp
		}
		if !s.hub.send(vi.NodeID, proto.TypeVolumeSnapshot, proto.VolumeOp{ID: id, Volumes: vms, Image: g.Spec.Container.Image, Auth: auth, URL: rel}) {
			_, _ = s.st.DB.Exec(`UPDATE volume_snapshots SET status='failed', error='node went offline' WHERE id=?`, id)
		}
		s.st.AddEvent(p, g.ID, "", vi.NodeID, "volume.snapshot", "Snapshot "+id+" of "+group+" started by "+u.Name)
		writeJSON(w, http.StatusAccepted, s.snapshots(`WHERE id=?`, id)[0])
	}))
	mux.HandleFunc("GET /v1/projects/{project}/snapshots", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, args := `WHERE project=?`, []any{p}
		if g := r.URL.Query().Get("group"); g != "" {
			q += ` AND grp=?`
			args = append(args, g)
		}
		writeJSON(w, http.StatusOK, api.List[Snapshot]{Items: s.snapshots(q+` ORDER BY created DESC`, args...)})
	}))
	mux.HandleFunc("GET /v1/projects/{project}/snapshots/{id}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		sn := s.snapshots(`WHERE project=? AND id=?`, p, r.PathValue("id"))
		if len(sn) == 0 {
			problem(w, http.StatusNotFound, "not found", "snapshot not found")
			return
		}
		writeJSON(w, http.StatusOK, sn[0])
	}))
	mux.HandleFunc("DELETE /v1/projects/{project}/snapshots/{id}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		sn := s.snapshots(`WHERE project=? AND id=?`, p, r.PathValue("id"))
		if len(sn) == 0 {
			problem(w, http.StatusNotFound, "not found", "snapshot not found")
			return
		}
		fp := snapshotPath(sn[0].Group, sn[0].ID)
		_ = os.Remove(s.filePath(p, fp))
		_, _ = s.st.DB.Exec(`DELETE FROM files WHERE project=? AND path=?`, p, fp)
		_, _ = s.st.DB.Exec(`DELETE FROM volume_snapshots WHERE id=?`, sn[0].ID)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /v1/projects/{project}/snapshots/{id}/restore", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var body struct {
			Group   string `json:"group"`   // default: the snapshot's group
			Replica *int   `json:"replica"` // default: the snapshot's replica
			Node    string `json:"node"`    // node id or name; default: the replica's pinned node, else the snapshot's node
		}
		if !decode(w, r, &body) {
			return
		}
		sns := s.snapshots(`WHERE project=? AND id=?`, p, r.PathValue("id"))
		if len(sns) == 0 {
			problem(w, http.StatusNotFound, "not found", "snapshot not found")
			return
		}
		sn := sns[0]
		if sn.Status != "ready" {
			problem(w, http.StatusConflict, "snapshot not ready", "status is "+sn.Status)
			return
		}
		group, replica := sn.Group, sn.Replica
		if body.Group != "" {
			group = body.Group
		}
		if body.Replica != nil {
			replica = *body.Replica
		}
		g, err := s.st.Group(p, group)
		if err != nil {
			problem(w, http.StatusNotFound, "not found", "group "+group+" not found (deploy it stopped, then restore)")
			return
		}
		var n int
		_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM instances WHERE group_id=? AND replica_index=? AND state!='terminated'`, g.ID, replica).Scan(&n)
		if n > 0 {
			problem(w, http.StatusConflict, "volume in use", fmt.Sprintf("replica %d of %s is running; stop the group first", replica, group))
			return
		}
		node := body.Node
		if node == "" {
			node = s.volumePin(p, group, replica)
		}
		if node == "" {
			node = sn.NodeID
		}
		if nr, err := s.st.NodeByID(node); err == nil {
			node = nr.ID
		} else if nr, err := s.st.NodeByName(node); err == nil {
			node = nr.ID
		} else {
			problem(w, http.StatusNotFound, "not found", "node "+node+" not found")
			return
		}
		if s.hub.get(node) == nil {
			problem(w, http.StatusConflict, "node offline", "target node is offline")
			return
		}
		// Volumes are matched by name; the target group must declare them.
		var vms []proto.VolumeMount
		var names []string
		for _, v := range g.Spec.Container.Volumes {
			vm := proto.VolumeMount{Name: v.Name, Source: volumeName(p, group, v.Name, replica), Target: v.Mount}
			vms = append(vms, vm)
			names = append(names, vm.Source)
		}
		if len(vms) == 0 {
			problem(w, http.StatusConflict, "no volumes", "group "+group+" declares no container.volumes")
			return
		}
		// If the replica's old volumes live on another node, remove them there (best effort).
		if old := s.volumePin(p, group, replica); old != "" && old != node {
			s.hub.send(old, proto.TypeVolumeDelete, proto.VolumeDelete{Names: names})
		}
		opID := sn.ID + ":" + store.NewSecret(4)
		restoreMu.Lock()
		restores[opID] = pendingRestore{project: p, group: group, replica: replica, node: node, names: names}
		restoreMu.Unlock()
		fp := snapshotPath(sn.Group, sn.ID)
		exp := time.Now().Add(6 * time.Hour).Unix()
		rel := fmt.Sprintf("/v1/signed/%s/%s?method=GET&exp=%d&sig=%s", p, fp, exp, s.signFile("GET", p, fp, exp))
		auth := g.Spec.Container.RegistryAuth
		if auth != nil {
			cp := *auth
			cp.Password, _ = s.expandSecrets(p, cp.Password)
			auth = &cp
		}
		_, _ = s.st.DB.Exec(`UPDATE volume_snapshots SET restore_status='restoring' WHERE id=?`, sn.ID)
		if !s.hub.send(node, proto.TypeVolumeRestore, proto.VolumeOp{ID: opID, Volumes: vms, Image: g.Spec.Container.Image, Auth: auth, URL: rel}) {
			problem(w, http.StatusConflict, "node offline", "target node went offline")
			return
		}
		s.st.AddEvent(p, g.ID, "", node, "volume.restore", "Restore of "+sn.ID+" into "+group+" replica "+strconv.Itoa(replica)+" started by "+u.Name)
		writeJSON(w, http.StatusAccepted, map[string]any{"snapshot": sn.ID, "group": group, "replica": replica, "node_id": node, "status": "restoring"})
	}))
}
