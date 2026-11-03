package enigmoss

import "context"

// Project is the project the request was signed for.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// KeyType is the key that signed the request: "payment" or "payout".
	KeyType string `json:"key_type"`
	// Permissions are enabled for the project and reachable with this key type, such as
	// "payout_create".
	Permissions []string `json:"permissions"`
	// ServerTime is the server Unix time; compare it with your clock.
	ServerTime int64 `json:"server_time"`
}

var epProjectGet = get("/project", scopeAny, "the project")

// ProjectService reads the project the key belongs to. Either key.
type ProjectService struct{ c *core }

// Get returns the project, the type of the key and its permissions. It changes nothing:
// the first safe call to check the keys, the permissions and the clock. GET /api/v4/project.
func (s *ProjectService) Get(ctx context.Context) (*Project, error) {
	return one[Project](ctx, s.c, call{ep: epProjectGet})
}
