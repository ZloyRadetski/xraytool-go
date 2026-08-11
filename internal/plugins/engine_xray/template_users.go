package engine_xray

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"xraytool/internal/domain"
)

// TemplateUserSnapshot reads hardcoded users from the configured Xray template
// without modifying it. The returned values deliberately use VPNUserConfig so
// cluster replication handles them exactly like database users.
func (a *Adapter) TemplateUserSnapshot(_ context.Context, managedUsers []domain.VPNUserConfig) ([]domain.VPNUserConfig, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.templateUserSnapshotLocked(managedUsers)
}

func (a *Adapter) templateUserSnapshotLocked(managedUsers []domain.VPNUserConfig) ([]domain.VPNUserConfig, error) {
	path := strings.TrimSpace(a.templatePath)
	if path == "" {
		return nil, nil
	}

	template, err := Read(path)
	if err != nil {
		return nil, fmt.Errorf("read template user source %q: %w", path, err)
	}
	return templateUsers(template, managedUsers, a.blacklistedAdmins)
}

// templateUsers converts email-bearing template clients into the domain user
// representation. A database user with the same email wins over a hardcoded
// template user, matching the normal config-generation collision policy.
func templateUsers(template RawConfig, managedUsers []domain.VPNUserConfig, blacklistedAdmins []string) ([]domain.VPNUserConfig, error) {
	managed := make(map[string]struct{}, len(managedUsers))
	for _, user := range managedUsers {
		if email := strings.TrimSpace(user.Email); email != "" {
			managed[email] = struct{}{}
		}
	}
	blacklisted := make(map[string]struct{}, len(blacklistedAdmins))
	for _, email := range blacklistedAdmins {
		if email = strings.TrimSpace(email); email != "" {
			blacklisted[email] = struct{}{}
		}
	}

	rawUsers, err := ListUsers(template)
	if err != nil {
		return nil, fmt.Errorf("list template users: %w", err)
	}

	users := make([]domain.VPNUserConfig, 0, len(rawUsers))
	for _, raw := range rawUsers {
		user, ok := rawClientToVPNUserConfig(raw)
		if !ok {
			continue
		}
		if _, exists := managed[user.Email]; exists {
			continue
		}
		if _, blocked := blacklisted[user.Email]; blocked {
			continue
		}
		users = append(users, user)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
	return users, nil
}

func rawClientToVPNUserConfig(client RawClient) (domain.VPNUserConfig, bool) {
	email := strings.TrimSpace(client.Email())
	if email == "" {
		return domain.VPNUserConfig{}, false
	}

	auth := client.GetString("auth")
	if auth == "" {
		auth = client.GetString("password")
	}
	maxDevices := 0
	if limit, ok := client.GetNumber("limit"); ok && limit > 0 {
		maxDevices = int(limit)
	}

	return domain.VPNUserConfig{
		Email:      email,
		UUID:       client.GetString("id"),
		Auth:       auth,
		Subfile:    client.GetString("subfile"),
		Expire:     client.GetString("expire"),
		MaxDevices: maxDevices,
		Flow:       client.GetString("flow"),
		Cipher:     client.GetString("method"),
	}, true
}

// withTemplateUsers makes the local, non-replication SyncUsers path retain
// hardcoded template users without giving it a separate configuration format.
// Replicated reconciliation intentionally skips this helper: its snapshot has
// already been assembled by the master and must replace slave-local users.
func (a *Adapter) withTemplateUsers(users []domain.VPNUserConfig) ([]domain.VPNUserConfig, error) {
	templateUsers, err := a.templateUserSnapshotLocked(users)
	if err != nil {
		return nil, err
	}
	if len(templateUsers) == 0 {
		return users, nil
	}
	return append(append([]domain.VPNUserConfig(nil), users...), templateUsers...), nil
}

func (a *Adapter) getProtectedTemplateUsers() map[string]bool {
	protected := make(map[string]bool)
	users, err := a.templateUserSnapshotLocked(nil)
	if err != nil {
		a.log.Warn("xray adapter: cannot read protected template users", "err", err)
		return protected
	}
	for _, user := range users {
		protected[user.Email] = true
	}
	return protected
}

// removeLegacyStaticClientState removes the obsolete sidecar left by versions
// that overlaid replicated template clients onto config.json. It is no longer
// read or written; all users now live in the regular replication snapshot.
func (a *Adapter) removeLegacyStaticClientState() {
	path := a.configPath + ".static-clients.json"
	if err := os.Remove(path); err == nil {
		a.log.Info("xray adapter: removed obsolete static-client state", "path", path)
	} else if !os.IsNotExist(err) {
		a.log.Warn("xray adapter: could not remove obsolete static-client state", "path", path, "err", err)
	}
}

var _ domain.TemplateUserSnapshotter = (*Adapter)(nil)
