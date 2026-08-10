package engine_xray

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	json "github.com/goccy/go-json"
	"os"
	"sort"
	"strings"

	"xraytool/internal/domain"
	"xraytool/internal/safeio"
)

// StaticClientSnapshot exports exactly the clients that are hardcoded in the
// Xray template. They remain opaque JSON because a static client may contain
// any protocol-specific fields supported by Xray.
//
// Installations without a template use config.json as the source. In that
// mode database-managed users are excluded by email, so only config-only
// clients are replicated.
func (a *Adapter) StaticClientSnapshot(_ context.Context, managedUsers []domain.VPNUserConfig) ([]domain.StaticInboundClients, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	sourcePath := a.staticClientsSourcePath()
	cfg, err := Read(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("read static client source %q: %w", sourcePath, err)
	}

	managedEmails := make(map[string]struct{}, len(managedUsers))
	for _, user := range managedUsers {
		if email := strings.TrimSpace(user.Email); email != "" {
			managedEmails[email] = struct{}{}
		}
	}
	blacklisted := a.blacklistedEmails()

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, fmt.Errorf("read static client inbounds: %w", err)
	}

	snapshot := make([]domain.StaticInboundClients, 0, len(inbounds))
	for _, inbound := range inbounds {
		if !inbound.HasClientList() || inbound.Tag() == "" {
			continue
		}

		clients, err := inbound.GetClients()
		if err != nil {
			return nil, fmt.Errorf("read clients for inbound %q: %w", inbound.Tag(), err)
		}

		staticClients := make([]RawClient, 0, len(clients))
		for _, client := range clients {
			email := client.Email()
			if email != "" {
				if _, managed := managedEmails[email]; managed {
					continue
				}
				if blacklisted[email] {
					continue
				}
			}
			staticClients = append(staticClients, client)
		}

		rawClients, err := json.Marshal(staticClients)
		if err != nil {
			return nil, fmt.Errorf("marshal clients for inbound %q: %w", inbound.Tag(), err)
		}
		snapshot = append(snapshot, domain.StaticInboundClients{
			InboundTag: inbound.Tag(),
			Protocol:   inbound.Protocol(),
			Clients:    rawClients,
		})
	}
	if a.templatePath == "" {
		// A direct config has no separate source file from which SyncUsers can
		// recognise static clients later. Persist the filtered snapshot as the
		// protected set before removeOrphans is allowed to run.
		if err := a.writeStaticClientState(snapshot); err != nil {
			return nil, err
		}
	}

	return snapshot, nil
}

// ApplyStaticClientSnapshot updates static clients on a slave without
// replacing the dynamic clients which are managed by the subscription DB.
// The active config is rebuilt immediately and the affected inbounds are
// hot-reloaded, therefore a state-hash match cannot leave new static clients
// waiting for the next full database snapshot.
func (a *Adapter) ApplyStaticClientSnapshot(ctx context.Context, snapshot []domain.StaticInboundClients) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	replacements, err := parseStaticClientSnapshot(snapshot)
	if err != nil {
		return err
	}
	if len(replacements) == 0 {
		return nil
	}

	var previous map[string][]RawClient
	if a.templatePath != "" {
		template, err := Read(a.templatePath)
		if err != nil {
			return fmt.Errorf("read template %q: %w", a.templatePath, err)
		}
		previous, err = staticClientsByInbound(template)
		if err != nil {
			return err
		}

		changed, skipped, err := replaceStaticClients(template, replacements, nil)
		if err != nil {
			return fmt.Errorf("apply template static clients: %w", err)
		}
		for _, tag := range skipped {
			a.log.Warn("xray adapter: static client snapshot inbound is absent on this node", "tag", tag)
		}
		if len(changed) > 0 {
			if err := Write(a.templatePath, template); err != nil {
				return fmt.Errorf("write template %q: %w", a.templatePath, err)
			}
		}
	} else {
		previous, err = a.readStaticClientState()
		if err != nil {
			return err
		}
	}

	changed, skipped, err := a.replaceActiveStaticClients(replacements, previous, a.templatePath == "")
	if err != nil {
		return err
	}
	for _, tag := range skipped {
		a.log.Warn("xray adapter: static client snapshot inbound is absent on this node", "tag", tag)
	}

	if a.templatePath == "" {
		if err := a.writeStaticClientState(snapshot); err != nil {
			return err
		}
	}

	if len(changed) == 0 {
		return nil
	}

	for _, tag := range changed {
		if err := a.rebuildInboundLocked(ctx, tag); err != nil {
			return fmt.Errorf("rebuild static-client inbound %q: %w", tag, err)
		}
	}

	a.invalidateHash()
	a.notifyConfigModified()
	a.log.Info("xray adapter: applied static client snapshot", "inbounds", len(changed))
	return nil
}

func (a *Adapter) staticClientsSourcePath() string {
	if a.templatePath != "" {
		return a.templatePath
	}
	return a.configPath
}

func (a *Adapter) blacklistedEmails() map[string]bool {
	blacklisted := make(map[string]bool, len(a.blacklistedAdmins))
	for _, email := range a.blacklistedAdmins {
		if email = strings.TrimSpace(email); email != "" {
			blacklisted[email] = true
		}
	}
	return blacklisted
}

type staticClientReplacement struct {
	protocol string
	clients  []RawClient
}

func parseStaticClientSnapshot(snapshot []domain.StaticInboundClients) (map[string]staticClientReplacement, error) {
	replacements := make(map[string]staticClientReplacement, len(snapshot))
	for _, inbound := range snapshot {
		tag := strings.TrimSpace(inbound.InboundTag)
		if tag == "" {
			return nil, fmt.Errorf("static client snapshot contains an inbound without tag")
		}
		if _, exists := replacements[tag]; exists {
			return nil, fmt.Errorf("static client snapshot contains duplicate inbound tag %q", tag)
		}

		clients := make([]RawClient, 0)
		if len(inbound.Clients) > 0 && string(inbound.Clients) != "null" {
			if err := json.Unmarshal(inbound.Clients, &clients); err != nil {
				return nil, fmt.Errorf("static client snapshot inbound %q has invalid clients: %w", tag, err)
			}
		}
		if clients == nil {
			clients = make([]RawClient, 0)
		}
		for index, client := range clients {
			if client == nil {
				return nil, fmt.Errorf("static client snapshot inbound %q has null client at index %d", tag, index)
			}
		}

		replacements[tag] = staticClientReplacement{
			protocol: strings.ToLower(strings.TrimSpace(inbound.Protocol)),
			clients:  clients,
		}
	}
	return replacements, nil
}

func staticClientsByInbound(cfg RawConfig) (map[string][]RawClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, fmt.Errorf("read static client inbounds: %w", err)
	}

	result := make(map[string][]RawClient, len(inbounds))
	for _, inbound := range inbounds {
		if !inbound.HasClientList() || inbound.Tag() == "" {
			continue
		}
		clients, err := inbound.GetClients()
		if err != nil {
			return nil, fmt.Errorf("read clients for inbound %q: %w", inbound.Tag(), err)
		}
		result[inbound.Tag()] = clients
	}
	return result, nil
}

// replaceStaticClients replaces the source/static list. previous is ignored:
// the template is itself the source of truth and contains no dynamic clients.
func replaceStaticClients(cfg RawConfig, replacements map[string]staticClientReplacement, _ map[string][]RawClient) ([]string, []string, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, nil, fmt.Errorf("read inbounds: %w", err)
	}

	found := make(map[string]bool, len(replacements))
	changed := make([]string, 0, len(replacements))
	for index, inbound := range inbounds {
		replacement, exists := replacements[inbound.Tag()]
		if !exists {
			continue
		}
		found[inbound.Tag()] = true
		if !inbound.HasClientList() {
			continue
		}
		if replacement.protocol != "" && replacement.protocol != inbound.Protocol() {
			return nil, nil, fmt.Errorf("inbound %q protocol mismatch: master=%q slave=%q", inbound.Tag(), replacement.protocol, inbound.Protocol())
		}

		current, err := inbound.GetClients()
		if err != nil {
			return nil, nil, fmt.Errorf("read clients for inbound %q: %w", inbound.Tag(), err)
		}
		if rawClientListsEqual(current, replacement.clients) {
			continue
		}
		if err := inbounds[index].SetClients(replacement.clients); err != nil {
			return nil, nil, fmt.Errorf("set clients for inbound %q: %w", inbound.Tag(), err)
		}
		changed = append(changed, inbound.Tag())
	}

	if len(changed) > 0 {
		if err := cfg.SetInbounds(inbounds); err != nil {
			return nil, nil, fmt.Errorf("write inbounds: %w", err)
		}
	}
	return changed, missingStaticTags(replacements, found), nil
}

func (a *Adapter) replaceActiveStaticClients(replacements map[string]staticClientReplacement, previous map[string][]RawClient, removeFirstMatchingEmails bool) ([]string, []string, error) {
	changed := make([]string, 0, len(replacements))
	skipped := make([]string, 0)
	err := Modify(a.configPath, func(cfg RawConfig) error {
		inbounds, err := cfg.GetInbounds()
		if err != nil {
			return fmt.Errorf("read inbounds: %w", err)
		}

		found := make(map[string]bool, len(replacements))
		for index, inbound := range inbounds {
			replacement, exists := replacements[inbound.Tag()]
			if !exists {
				continue
			}
			found[inbound.Tag()] = true
			if !inbound.HasClientList() {
				continue
			}
			if replacement.protocol != "" && replacement.protocol != inbound.Protocol() {
				return fmt.Errorf("inbound %q protocol mismatch: master=%q slave=%q", inbound.Tag(), replacement.protocol, inbound.Protocol())
			}

			current, err := inbound.GetClients()
			if err != nil {
				return fmt.Errorf("read clients for inbound %q: %w", inbound.Tag(), err)
			}
			merged := mergeStaticClients(current, previous[inbound.Tag()], replacement.clients, removeFirstMatchingEmails)
			if rawClientListsEqual(current, merged) {
				continue
			}
			if err := inbounds[index].SetClients(merged); err != nil {
				return fmt.Errorf("set clients for inbound %q: %w", inbound.Tag(), err)
			}
			changed = append(changed, inbound.Tag())
		}

		skipped = missingStaticTags(replacements, found)
		if len(changed) == 0 {
			return nil
		}
		if err := cfg.SetInbounds(inbounds); err != nil {
			return fmt.Errorf("write inbounds: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("apply active static clients: %w", err)
	}
	return changed, skipped, nil
}

func mergeStaticClients(current, previous, desired []RawClient, removeFirstMatchingEmails bool) []RawClient {
	previousKeys := rawClientKeySet(previous)
	desiredEmails := make(map[string]bool, len(desired))
	for _, client := range desired {
		if email := client.Email(); email != "" {
			desiredEmails[email] = true
		}
	}

	kept := make([]RawClient, 0, len(current)+len(desired))
	keptKeys := make(map[string]bool, len(current)+len(desired))
	keptEmails := make(map[string]bool, len(current))
	for _, client := range current {
		key := rawClientKey(client)
		if previousKeys[key] {
			continue
		}
		// On the first direct-config sync there is no previous state. Replacing
		// a same-email hardcoded client avoids duplicate Xray users while keeping
		// unrelated local clients intact.
		if removeFirstMatchingEmails && client.Email() != "" && desiredEmails[client.Email()] {
			continue
		}
		kept = append(kept, client)
		keptKeys[key] = true
		if email := client.Email(); email != "" {
			keptEmails[email] = true
		}
	}

	for _, client := range desired {
		key := rawClientKey(client)
		if keptKeys[key] {
			continue
		}
		// A database user already present in the active config wins over a
		// colliding hardcoded email. The master excludes its DB users from the
		// snapshot, but this additionally protects a temporarily stale slave.
		if email := client.Email(); email != "" && keptEmails[email] {
			continue
		}
		kept = append(kept, client)
		keptKeys[key] = true
		if email := client.Email(); email != "" {
			keptEmails[email] = true
		}
	}
	return kept
}

func rawClientListsEqual(left, right []RawClient) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if rawClientKey(left[index]) != rawClientKey(right[index]) {
			return false
		}
	}
	return true
}

func rawClientKeySet(clients []RawClient) map[string]bool {
	keys := make(map[string]bool, len(clients))
	for _, client := range clients {
		keys[rawClientKey(client)] = true
	}
	return keys
}

func rawClientKey(client RawClient) string {
	data, err := json.Marshal(client)
	if err != nil {
		return "invalid-client"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func missingStaticTags(replacements map[string]staticClientReplacement, found map[string]bool) []string {
	missing := make([]string, 0)
	for tag := range replacements {
		if !found[tag] {
			missing = append(missing, tag)
		}
	}
	sort.Strings(missing)
	return missing
}

func (a *Adapter) staticClientStatePath() string {
	return a.configPath + ".static-clients.json"
}

func (a *Adapter) readStaticClientState() (map[string][]RawClient, error) {
	data, err := os.ReadFile(a.staticClientStatePath())
	if os.IsNotExist(err) {
		return make(map[string][]RawClient), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read static client state: %w", err)
	}
	var snapshot []domain.StaticInboundClients
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("parse static client state: %w", err)
	}
	replacements, err := parseStaticClientSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	state := make(map[string][]RawClient, len(replacements))
	for tag, replacement := range replacements {
		state[tag] = replacement.clients
	}
	return state, nil
}

func (a *Adapter) writeStaticClientState(snapshot []domain.StaticInboundClients) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal static client state: %w", err)
	}
	path := a.staticClientStatePath()
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	if err := safeio.WriteToFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write static client state: %w", err)
	}
	return nil
}

var _ domain.StaticClientSynchronizer = (*Adapter)(nil)
