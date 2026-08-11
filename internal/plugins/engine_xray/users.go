package engine_xray

import (
	"fmt"
	json "github.com/goccy/go-json"
	"os"
)

// ---------------------------------------------------------------------------
// User queries
// ---------------------------------------------------------------------------

// UserExists returns true if any inbound contains a client with the given email.
func UserExists(cfg RawConfig, email string) (bool, error) {
	c, err := FindUser(cfg, email)
	return c != nil, err
}

// FindUser returns a merged representation of all clients matching email across all inbounds, or nil.
func FindUser(cfg RawConfig, email string) (RawClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}
	var matched []RawClient
	for _, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] xrayconfig: пропуск inbound из-за ошибки парсинга: %v\n", err)
			continue
		}
		for _, c := range clients {
			if c.Email() == email {
				matched = append(matched, c)
			}
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	return mergeClients(matched), nil
}

// BuildUserIndex creates an index of users by email to avoid repeated
// parsing of the config in hot loops. It returns a lookup function.
func BuildUserIndex(cfg RawConfig) (func(email string) RawClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	index := make(map[string][]RawClient)
	for _, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] xrayconfig: пропуск inbound из-за ошибки парсинга: %v\n", err)
			continue
		}
		for _, c := range clients {
			email := c.Email()
			// match FindUser behavior: even if email is empty string, we might index it if we search for it?
			// Actually FindUser checks if c.Email() == email, so it matches empty emails if we pass "".
			// Let's just index everything.
			index[email] = append(index[email], c)
		}
	}

	return func(email string) RawClient {
		matched := index[email]
		if len(matched) == 0 {
			return nil
		}
		return mergeClients(matched)
	}, nil
}

// ListUsers returns one entry per unique email, merging their fields across all inbounds.
func ListUsers(cfg RawConfig) ([]RawClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	groups := make(map[string][]RawClient)
	var order []string

	for _, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] xrayconfig: пропуск inbound из-за ошибки парсинга: %v\n", err)
			continue
		}
		for _, c := range clients {
			e := c.Email()
			if e == "" {
				continue
			}
			if len(groups[e]) == 0 {
				order = append(order, e)
			}
			groups[e] = append(groups[e], c)
		}
	}

	result := make([]RawClient, 0, len(order))
	for _, e := range order {
		result = append(result, mergeClients(groups[e]))
	}
	return result, nil
}

func mergeClients(clients []RawClient) RawClient {
	if len(clients) == 0 {
		return nil
	}
	merged := make(RawClient)
	for _, c := range clients {
		for k, v := range c {
			if isRawMessageEmpty(v) {
				if _, ok := merged[k]; !ok {
					merged[k] = v
				}
			} else {
				merged[k] = v
			}
		}
	}
	return merged
}

func isRawMessageEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	s := string(raw)
	return s == "null" || s == `""` || s == ""
}

// InboundTagsForUser returns the tags of all inbounds that contain this user.
func InboundTagsForUser(cfg RawConfig, email string) ([]string, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] xrayconfig: пропуск inbound из-за ошибки парсинга: %v\n", err)
			continue
		}
		for _, c := range clients {
			if c.Email() == email {
				if tag := ib.Tag(); tag != "" {
					tags = append(tags, tag)
				}
				break
			}
		}
	}
	return tags, nil
}

// InboundTagsForUsers returns the tags of all inbounds that contain these users.
func InboundTagsForUsers(cfg RawConfig, emails []string) (map[string][]string, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	emailMap := make(map[string]bool, len(emails))
	for _, e := range emails {
		emailMap[e] = true
	}

	result := make(map[string][]string)
	for _, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] xrayconfig: пропуск inbound из-за ошибки парсинга: %v\n", err)
			continue
		}
		foundInInbound := make(map[string]bool)
		for _, c := range clients {
			e := c.Email()
			if emailMap[e] && !foundInInbound[e] {
				foundInInbound[e] = true
				if tag := ib.Tag(); tag != "" {
					result[e] = append(result[e], tag)
				}
			}
		}
	}
	return result, nil
}

// ClientInbounds returns info about all inbounds that have a client list.
func ClientInbounds(cfg RawConfig) ([]InboundInfo, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}
	var result []InboundInfo
	for _, ib := range inbounds {
		if ib.HasClientList() && ib.Tag() != "" {
			result = append(result, InboundInfo{Tag: ib.Tag(), Protocol: ib.Protocol()})
		}
	}
	return result, nil
}

// MissingClientPayload returns only the requested client entries that are absent
// from their respective inbound. A client in one inbound must never make the
// same user appear present in another inbound.
func MissingClientPayload(cfg RawConfig, payload []TaggedClient) ([]TaggedClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	clientsByTag := make(map[string]map[string]bool, len(inbounds))
	for _, inbound := range inbounds {
		clients, err := inbound.GetClients()
		if err != nil {
			return nil, fmt.Errorf("inbound %q: %w", inbound.Tag(), err)
		}
		if clients == nil || inbound.Tag() == "" {
			continue
		}
		emails := make(map[string]bool, len(clients))
		for _, client := range clients {
			if email := client.Email(); email != "" {
				emails[email] = true
			}
		}
		clientsByTag[inbound.Tag()] = emails
	}

	missing := make([]TaggedClient, 0, len(payload))
	for _, tagged := range payload {
		if tagged.Tag == "" || tagged.Client == nil || tagged.Client.Email() == "" {
			continue
		}
		if !clientsByTag[tagged.Tag][tagged.Client.Email()] {
			missing = append(missing, tagged)
		}
	}
	return missing, nil
}

// DesiredUserClientPayload returns the exact generated client entries for the
// supplied dynamic users. Unlike ListUsers it preserves the inbound tag, which
// is required to reconcile a partially applied Xray configuration.
func DesiredUserClientPayload(cfg RawConfig, emails map[string]bool) ([]TaggedClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	payload := make([]TaggedClient, 0)
	for _, inbound := range inbounds {
		if inbound.Tag() == "" {
			continue
		}
		clients, err := inbound.GetClients()
		if err != nil {
			return nil, fmt.Errorf("inbound %q: %w", inbound.Tag(), err)
		}
		for _, client := range clients {
			if email := client.Email(); email != "" && emails[email] {
				payload = append(payload, TaggedClient{Tag: inbound.Tag(), Client: client})
			}
		}
	}
	return payload, nil
}

// DiffClientPayload compares the previous on-disk configuration with the
// desired clients. It tracks membership by both inbound tag and email instead
// of collapsing all inbound state into one record per user.
func DiffClientPayload(previous RawConfig, desired []TaggedClient) (adds []TaggedClient, removals map[string][]string, err error) {
	removals = make(map[string][]string)
	if previous == nil {
		return append([]TaggedClient(nil), desired...), removals, nil
	}

	inbounds, err := previous.GetInbounds()
	if err != nil {
		return nil, nil, err
	}
	previousByTag := make(map[string]map[string]RawClient, len(inbounds))
	for _, inbound := range inbounds {
		clients, err := inbound.GetClients()
		if err != nil {
			return nil, nil, fmt.Errorf("inbound %q: %w", inbound.Tag(), err)
		}
		if clients == nil || inbound.Tag() == "" {
			continue
		}
		byEmail := make(map[string]RawClient, len(clients))
		for _, client := range clients {
			if email := client.Email(); email != "" {
				byEmail[email] = client
			}
		}
		previousByTag[inbound.Tag()] = byEmail
	}

	for _, tagged := range desired {
		if tagged.Tag == "" || tagged.Client == nil || tagged.Client.Email() == "" {
			continue
		}
		email := tagged.Client.Email()
		previousClient, exists := previousByTag[tagged.Tag][email]
		if !exists {
			adds = append(adds, tagged)
			continue
		}
		if rawClientKey(previousClient) != rawClientKey(tagged.Client) {
			removals[email] = append(removals[email], tagged.Tag)
			adds = append(adds, tagged)
		}
	}
	return adds, removals, nil
}

// ---------------------------------------------------------------------------
// User mutations (operate on a RawConfig in memory; caller writes to disk)
// ---------------------------------------------------------------------------

// AddUserToInbounds appends a new client to every inbound that has a client
// list, using the pre-built TaggedClient payload.
func AddUserToInbounds(cfg RawConfig, payload []TaggedClient) error {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}

	byTag := taggedClientMap(payload)

	for i, ib := range inbounds {
		tag := ib.Tag()
		newClient, ok := byTag[tag]
		if !ok {
			continue
		}
		existing, err := ib.GetClients()
		if err != nil {
			return fmt.Errorf("inbound %q: %w", tag, err)
		}
		if existing == nil {
			continue // not a client inbound
		}

		newEmail := newClient.Email()
		var kept []RawClient
		for _, c := range existing {
			if c.Email() == "" || c.Email() != newEmail {
				kept = append(kept, c)
			}
		}
		kept = append(kept, newClient) //nolint:gocritic

		if err := inbounds[i].SetClients(kept); err != nil {
			return fmt.Errorf("inbound %q: %w", tag, err)
		}
	}

	return cfg.SetInbounds(inbounds)
}

// RemoveUserFromAllInbounds removes the client with the given email from every inbound.
func RemoveUserFromAllInbounds(cfg RawConfig, email string) error {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}

	for i, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			return err
		}
		if clients == nil {
			continue
		}
		var kept []RawClient
		for _, c := range clients {
			if c.Email() != email {
				kept = append(kept, c)
			}
		}
		if err := inbounds[i].SetClients(kept); err != nil {
			return err
		}
	}

	return cfg.SetInbounds(inbounds)
}

// RemoveUsersFromAllInbounds removes the clients with the given emails from every inbound.
func RemoveUsersFromAllInbounds(cfg RawConfig, emails []string) error {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}

	emailMap := make(map[string]bool, len(emails))
	for _, e := range emails {
		emailMap[e] = true
	}

	for i, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			return err
		}
		if clients == nil {
			continue
		}
		var kept []RawClient
		for _, c := range clients {
			if !emailMap[c.Email()] {
				kept = append(kept, c)
			}
		}
		if err := inbounds[i].SetClients(kept); err != nil {
			return err
		}
	}

	return cfg.SetInbounds(inbounds)
}

// UpdateStringField sets a string field on all clients matching email.
func UpdateStringField(cfg RawConfig, email, key, value string) error {
	return mutateClients(cfg, email, func(c RawClient) {
		c.Set(key, value)
	})
}

// UpdateNumberField sets a numeric field on all clients matching email.
func UpdateNumberField(cfg RawConfig, email, key string, value float64) error {
	return mutateClients(cfg, email, func(c RawClient) {
		c.SetNumber(key, value)
	})
}

// ReplaceAllClients replaces the entire client list in each inbound
// (used by migrate command).
func ReplaceAllClients(cfg RawConfig, payload []TaggedClient) error {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}

	// Group payload clients by tag.
	grouped := make(map[string][]RawClient)
	for _, tc := range payload {
		grouped[tc.Tag] = append(grouped[tc.Tag], tc.Client)
	}

	for i, ib := range inbounds {
		tag := ib.Tag()
		clients, ok := grouped[tag]
		if !ok || !ib.HasClientList() {
			continue
		}
		if err := inbounds[i].SetClients(clients); err != nil {
			return err
		}
	}

	return cfg.SetInbounds(inbounds)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func taggedClientMap(payload []TaggedClient) map[string]RawClient {
	m := make(map[string]RawClient, len(payload))
	for _, tc := range payload {
		m[tc.Tag] = tc.Client
	}
	return m
}

func mutateClients(cfg RawConfig, email string, fn func(RawClient)) error {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}
	for i, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			return err
		}
		if clients == nil {
			continue
		}
		changed := false
		for j, c := range clients {
			if c.Email() == email {
				fn(c)
				clients[j] = c.CleanLegacy()
				changed = true
			}
		}
		if changed {
			if err := inbounds[i].SetClients(clients); err != nil {
				return err
			}
		}
	}
	return cfg.SetInbounds(inbounds)
}
