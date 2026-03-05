package inhibitor

import (
	"context"
	"encoding/json"
	"log"

	"github.com/redis/go-redis/v9"
)

const (
	redisInhibitHash    = "power:inhibits"
	redisInhibitChannel = "power:inhibits"
)

// redisInhibitData mirrors the JSON structure written by update-service
type redisInhibitData struct {
	ID       string `json:"id"`
	Who      string `json:"who"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Type     string `json:"type"`
	Duration int64  `json:"duration"`
	Created  int64  `json:"created"`
}

// StartRedisListener subscribes to the power:inhibits channel and syncs
// inhibitors from the power:inhibits Redis hash into the Manager.
// Must be called as a goroutine.
func (m *Manager) StartRedisListener(ctx context.Context, client *redis.Client, logger *log.Logger) {
	// Initial sync: load any existing inhibitors from Redis hash
	m.syncRedisInhibitors(ctx, client, logger)

	// Subscribe to changes
	pubsub := client.Subscribe(ctx, redisInhibitChannel)
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			m.syncRedisInhibitors(ctx, client, logger)
		}
	}
}

// syncRedisInhibitors reads the power:inhibits hash and updates
// the manager's manual inhibitors to match.
func (m *Manager) syncRedisInhibitors(ctx context.Context, client *redis.Client, logger *log.Logger) {
	entries, err := client.HGetAll(ctx, redisInhibitHash).Result()
	if err != nil {
		logger.Printf("Failed to read %s: %v", redisInhibitHash, err)
		return
	}

	// Build set of desired Redis inhibitor IDs
	wantIDs := make(map[string]redisInhibitData)
	for id, raw := range entries {
		var data redisInhibitData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			logger.Printf("Failed to parse inhibitor %s: %v", id, err)
			continue
		}
		data.ID = id
		wantIDs[id] = data
	}

	changed := false

	m.mutex.Lock()

	// Remove manual inhibitors that are no longer in Redis
	remaining := make([]*Inhibitor, 0, len(m.manualInhibitors))
	for _, inh := range m.manualInhibitors {
		if inh.redisID == "" {
			remaining = append(remaining, inh)
			continue
		}
		if _, ok := wantIDs[inh.redisID]; ok {
			remaining = append(remaining, inh)
			delete(wantIDs, inh.redisID)
		} else {
			logger.Printf("Redis inhibitor removed: %s (%s)", inh.redisID, inh.Why)
			changed = true
		}
	}
	m.manualInhibitors = remaining

	// Add new inhibitors from Redis
	for id, data := range wantIDs {
		inhibType := TypeBlock
		if data.Type == "delay" {
			inhibType = TypeDelay
		}
		inh := &Inhibitor{
			Who:     data.Who,
			What:    data.What,
			Why:     data.Why,
			Type:    inhibType,
			redisID: id,
		}
		m.manualInhibitors = append(m.manualInhibitors, inh)
		logger.Printf("Redis inhibitor added: %s (%s) by %s — %s", id, data.Type, data.Who, data.Why)
		changed = true
	}

	m.mutex.Unlock()

	if changed && m.onChange != nil {
		m.onChange()
	}
}
