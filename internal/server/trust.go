package server

// Node trust. Whoever runs a node can inspect the containers on it: their
// environment (decrypted secrets) and the identity tokens IMDS hands them. So
// groups that use secrets, and hub-audience identity tokens, only go to nodes
// trusted for the group's project:
//   - nodes a cluster admin marked trusted (PATCH /v1/nodes/{node} {"trusted": true}),
//   - nodes owned by a cluster admin,
//   - nodes owned by the project's owner or one of its members.
// A group can opt out with allow_untrusted_nodes: true.

import (
	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const trustSchema = `CREATE TABLE IF NOT EXISTS node_trust (node_id TEXT PRIMARY KEY, created INTEGER NOT NULL);`

func (s *Server) initTrust() error {
	_, err := s.st.DB.Exec(trustSchema)
	return err
}

// nodeMarkedTrusted reports the admin-set trust flag.
func (s *Server) nodeMarkedTrusted(nodeID string) bool {
	var n int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM node_trust WHERE node_id=?`, nodeID).Scan(&n)
	return n > 0
}

func (s *Server) setNodeTrusted(nodeID string, on bool) {
	if on {
		_, _ = s.st.DB.Exec(`INSERT OR IGNORE INTO node_trust(node_id, created) VALUES(?,?)`, nodeID, store.Now())
	} else {
		_, _ = s.st.DB.Exec(`DELETE FROM node_trust WHERE node_id=?`, nodeID)
	}
}

// nodeTrustedFor reports whether node may receive the project's secrets and hub tokens.
func (s *Server) nodeTrustedFor(n *store.NodeRow, project string) bool {
	if s.nodeMarkedTrusted(n.ID) {
		return true
	}
	if u, err := s.st.UserByID(n.OwnerID); err == nil && u.Role == "admin" {
		return true
	}
	var c int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM projects p LEFT JOIN project_members m ON m.project=p.name AND m.user_id=?
		WHERE p.name=? AND (p.owner_id=? OR m.user_id IS NOT NULL)`, n.OwnerID, project, n.OwnerID).Scan(&c)
	return c > 0
}

// specUsesSecrets reports whether a group's spec references any secret.
func specUsesSecrets(sp api.ContainerGroupSpec) bool {
	for _, v := range sp.Container.Env {
		for _, ref := range api.ParseRefs(v) {
			if ref.Kind == "secret" {
				return true
			}
		}
	}
	if a := sp.Container.RegistryAuth; a != nil {
		for _, ref := range api.ParseRefs(a.Password) {
			if ref.Kind == "secret" {
				return true
			}
		}
	}
	return false
}

// needsTrustedNode reports whether the group may only run on trusted nodes.
func needsTrustedNode(sp api.ContainerGroupSpec) bool {
	return !sp.AllowUntrustedNodes && specUsesSecrets(sp)
}
