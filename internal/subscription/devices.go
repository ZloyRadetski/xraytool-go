package subscription

import (
	"encoding/json"
	"os"
	"time"

	"xraytool/internal/logger"
	"xraytool/internal/safeio"
)

// DeviceItem struct exists in subscription.go, but we will redefine or use the existing one.
// Let's assume they remain defined in subscription.go to avoid conflict.

func (c *CacheManager) loadDeviceStateLocked() {
	if c.deviceStateLoaded {
		return
	}

	resolvedPath := resolveDeviceStatePath(c.cfg.Paths.DevicesState)

	// We use an advisory lock here just in case CLI is accessing it concurrently
	lf, err := os.OpenFile(resolvedPath+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err == nil {
		acquireFileLock(lf)
		defer func() {
			releaseFileLock(lf)
			lf.Close()
		}()
	}

	info, statErr := os.Stat(resolvedPath)
	data, err := os.ReadFile(resolvedPath)
	state := DeviceState{Clients: make(map[string]*ClientDevices)}
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &state); err != nil {
			logger.Warnf("[Cache] Предупреждение: не удалось прочитать devices_state.json: %v. Сброс состояния.", err)
		}
	}

	if state.Clients == nil {
		state.Clients = make(map[string]*ClientDevices)
	}

	c.deviceState = state
	c.deviceStateLoaded = true
	if statErr == nil {
		c.deviceStateModTime = info.ModTime()
	}
}

// refreshDeviceState checks if the external bot modified devices_state.json on disk and reloads it.
func (c *CacheManager) refreshDeviceState() {
	resolvedPath := resolveDeviceStatePath(c.cfg.Paths.DevicesState)
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return
	}

	c.deviceStateMu.Lock()
	defer c.deviceStateMu.Unlock()

	// If the server has pending writes, we skip reloading to avoid overwriting our own recent changes
	if c.deviceStateDirty {
		return
	}

	if !c.deviceStateLoaded || info.ModTime().After(c.deviceStateModTime) {
		logger.Infof("[Cache] Обнаружено изменение %s сторонним процессом (например, ботом). Перезагрузка состояний...", resolvedPath)
		c.deviceStateLoaded = false
		c.loadDeviceStateLocked()
	}
}

func (c *CacheManager) flushDeviceStateWorker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.FlushDeviceState()
		case <-c.done:
			return
		}
	}
}

// FlushDeviceState writes the device state to disk if it was modified.
func (c *CacheManager) FlushDeviceState() {
	c.deviceStateMu.Lock()
	if !c.deviceStateDirty {
		c.deviceStateMu.Unlock()
		return
	}

	payload, err := json.MarshalIndent(c.deviceState, "", "  ")
	c.deviceStateDirty = false
	c.deviceStateMu.Unlock()

	if err != nil {
		logger.Errorf("[Cache] Error marshaling device state: %v", err)
		return
	}

	resolvedPath := resolveDeviceStatePath(c.cfg.Paths.DevicesState)
	// Lock the file for safe writing across processes
	lf, err := os.OpenFile(resolvedPath+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err == nil {
		acquireFileLock(lf)
		defer func() {
			releaseFileLock(lf)
			lf.Close()
		}()
	}

	if err := safeio.WriteToFile(resolvedPath, payload, 0644); err != nil {
		c.deviceStateMu.Lock()
		c.deviceStateDirty = true
		c.deviceStateMu.Unlock()
		logger.Errorf("[Cache] writing device state to %q: %v", resolvedPath, err)
		return
	}

	// Update mod time so we don't reload our own write
	if info, err := os.Stat(resolvedPath); err == nil {
		c.deviceStateMu.Lock()
		c.deviceStateModTime = info.ModTime()
		c.deviceStateMu.Unlock()
	}
}

// Stop signals the background flush worker to exit and performs a final flush.
func (c *CacheManager) Stop() {
	close(c.done)
	c.FlushDeviceState()
}

// HasDeviceHistory returns true if the client has any device history.
func (c *CacheManager) HasDeviceHistory(filename, clientId string) bool {
	c.deviceStateMu.Lock()
	defer c.deviceStateMu.Unlock()
	c.loadDeviceStateLocked()

	variants := buildDeviceClientKeyVariants(filename, clientId)
	for _, v := range variants {
		if cd, ok := c.deviceState.Clients[v]; ok && cd != nil && len(cd.Devices) > 0 {
			return true
		}
	}
	return false
}

// UpdateDeviceState updates the device state in memory.
func (c *CacheManager) UpdateDeviceState(filename, clientId, hwid string, maxDevices int, model, osName, verOs, ua string) (bool, error) {
	c.deviceStateMu.Lock()
	defer c.deviceStateMu.Unlock()
	c.loadDeviceStateLocked()

	clientKey := canonicalDeviceClientKey(filename, clientId)
	if clientKey == "" {
		clientKey = filename
	}

	variants := buildDeviceClientKeyVariants(filename, clientId)
	if clientKey != "" {
		found := false
		for _, v := range variants {
			if v == clientKey {
				found = true
				break
			}
		}
		if !found {
			variants = append([]string{clientKey}, variants...)
		}
	}

	if c.deviceState.Clients[clientKey] == nil {
		c.deviceState.Clients[clientKey] = &ClientDevices{Devices: []DeviceItem{}}
	}

	// Gather all devices from variants
	var rawDevices []DeviceItem
	for _, variant := range variants {
		if cd, ok := c.deviceState.Clients[variant]; ok && cd != nil {
			rawDevices = append(rawDevices, cd.Devices...)
		}
	}

	// Deduplicate
	var devices []DeviceItem
	dedup := make(map[string]int)
	for _, dev := range rawDevices {
		hwidNorm := normalizeHwid(dev.Hwid)
		if hwidNorm == "" {
			continue
		}
		if idx, ok := dedup[hwidNorm]; ok {
			// Merge
			if dev.FirstSeen != "" && (devices[idx].FirstSeen == "" || dev.FirstSeen < devices[idx].FirstSeen) {
				devices[idx].FirstSeen = dev.FirstSeen
			}
			if dev.LastSeen != "" && (devices[idx].LastSeen == "" || dev.LastSeen > devices[idx].LastSeen) {
				devices[idx].LastSeen = dev.LastSeen
			}
			devices[idx].RequestCount += dev.RequestCount
			if dev.DeviceModel != "" {
				devices[idx].DeviceModel = dev.DeviceModel
			}
			if dev.DeviceOs != "" {
				devices[idx].DeviceOs = dev.DeviceOs
			}
			if dev.VerOs != "" {
				devices[idx].VerOs = dev.VerOs
			}
			if dev.LastUserAgent != "" {
				devices[idx].LastUserAgent = dev.LastUserAgent
			}
		} else {
			dedup[hwidNorm] = len(devices)
			dev.Hwid = hwidNorm
			devices = append(devices, dev)
		}
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05+00:00")
	existingIdx := -1
	for idx, dev := range devices {
		if dev.Hwid == hwid {
			existingIdx = idx
			break
		}
	}

	limitReached := false
	if existingIdx >= 0 {
		if devices[existingIdx].FirstSeen == "" {
			devices[existingIdx].FirstSeen = now
		}
		devices[existingIdx].LastSeen = now
		devices[existingIdx].RequestCount++
		devices[existingIdx].DeviceModel = model
		devices[existingIdx].DeviceOs = osName
		devices[existingIdx].VerOs = verOs
		devices[existingIdx].LastUserAgent = ua
	} else {
		if maxDevices > 0 && len(devices) >= maxDevices {
			limitReached = true
		} else {
			devices = append(devices, DeviceItem{
				Hwid:          hwid,
				FirstSeen:     now,
				LastSeen:      now,
				RequestCount:  1,
				DeviceModel:   model,
				DeviceOs:      osName,
				VerOs:         verOs,
				LastUserAgent: ua,
			})
		}
	}

	if !limitReached {
		c.deviceState.Clients[clientKey].Devices = devices
		// Clean other variants
		for _, variant := range variants {
			if variant != clientKey {
				delete(c.deviceState.Clients, variant)
			}
		}
		c.deviceStateDirty = true
	}

	return limitReached, nil
}
