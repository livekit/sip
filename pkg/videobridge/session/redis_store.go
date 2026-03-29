// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/redis/go-redis/v9"
)

const (
	redisKeyPrefix = "lk:sip-video:"
	sessionTTL     = 24 * time.Hour
	heartbeatTTL   = 30 * time.Second
)

// RedisStore persists session state in Redis for horizontal scaling.
// Multiple bridge instances share session awareness through Redis,
// enabling proper routing and preventing duplicate sessions.
type RedisStore struct {
	log    logger.Logger
	client redis.UniversalClient
	nodeID string // unique ID for this bridge instance
}

// RedisSessionRecord is the JSON structure stored in Redis per session.
type RedisSessionRecord struct {
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	RoomName  string `json:"room_name"`
	FromURI   string `json:"from_uri"`
	ToURI     string `json:"to_uri"`
	NodeID    string `json:"node_id"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	VideoPort int    `json:"video_port"`
	AudioPort int    `json:"audio_port"`
}

// NewRedisStore creates a new Redis-backed session store.
func NewRedisStore(log logger.Logger, addr, username, password string, db int, nodeID string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Username: username,
		Password: password,
		DB:       db,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	log.Infow("Redis session store connected", "addr", addr, "nodeID", nodeID)

	return &RedisStore{
		log:    log,
		client: client,
		nodeID: nodeID,
	}, nil
}

// Save persists a session record to Redis.
func (s *RedisStore) Save(ctx context.Context, sess *Session) error {
	record := RedisSessionRecord{
		SessionID: sess.ID,
		CallID:    sess.CallID,
		RoomName:  sess.RoomName,
		FromURI:   sess.FromURI,
		ToURI:     sess.ToURI,
		NodeID:    s.nodeID,
		State:     sess.State().String(),
		CreatedAt: sess.startTime.Unix(),
		UpdatedAt: time.Now().Unix(),
		VideoPort: sess.VideoPort(),
		AudioPort: sess.AudioPort(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshaling session record: %w", err)
	}

	key := s.sessionKey(sess.CallID)
	if err := s.client.Set(ctx, key, data, sessionTTL).Err(); err != nil {
		return fmt.Errorf("saving session to redis: %w", err)
	}

	return nil
}

// Load retrieves a session record from Redis by call ID.
func (s *RedisStore) Load(ctx context.Context, callID string) (*RedisSessionRecord, error) {
	key := s.sessionKey(callID)
	data, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading session from redis: %w", err)
	}

	var record RedisSessionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("unmarshaling session record: %w", err)
	}

	return &record, nil
}

// Delete removes a session record from Redis.
func (s *RedisStore) Delete(ctx context.Context, callID string) error {
	key := s.sessionKey(callID)
	return s.client.Del(ctx, key).Err()
}

// Exists checks if a session exists for the given call ID.
func (s *RedisStore) Exists(ctx context.Context, callID string) (bool, error) {
	key := s.sessionKey(callID)
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListAll returns all active session records across all nodes.
func (s *RedisStore) ListAll(ctx context.Context) ([]RedisSessionRecord, error) {
	pattern := redisKeyPrefix + "session:*"
	keys, err := s.client.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("listing session keys: %w", err)
	}

	if len(keys) == 0 {
		return nil, nil
	}

	vals, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("loading sessions: %w", err)
	}

	var records []RedisSessionRecord
	for _, v := range vals {
		if v == nil {
			continue
		}
		str, ok := v.(string)
		if !ok {
			continue
		}
		var rec RedisSessionRecord
		if err := json.Unmarshal([]byte(str), &rec); err != nil {
			s.log.Warnw("failed to unmarshal session record", err)
			continue
		}
		records = append(records, rec)
	}

	return records, nil
}

// CountByNode returns the number of active sessions on a specific node.
func (s *RedisStore) CountByNode(ctx context.Context, nodeID string) (int, error) {
	all, err := s.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range all {
		if r.NodeID == nodeID {
			count++
		}
	}
	return count, nil
}

// Heartbeat updates the node heartbeat in Redis.
// Used by other nodes to detect if a node is alive.
func (s *RedisStore) Heartbeat(ctx context.Context) error {
	key := redisKeyPrefix + "node:" + s.nodeID
	data := fmt.Sprintf(`{"node_id":"%s","ts":%d}`, s.nodeID, time.Now().Unix())
	return s.client.Set(ctx, key, data, heartbeatTTL).Err()
}

// StartHeartbeat begins a background loop that updates the node heartbeat.
func (s *RedisStore) StartHeartbeat(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(heartbeatTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Heartbeat(ctx); err != nil {
					s.log.Warnw("heartbeat failed", err)
				}
			}
		}
	}()
}

// CleanupStale removes sessions from dead nodes (no heartbeat for > heartbeatTTL).
func (s *RedisStore) CleanupStale(ctx context.Context) (int, error) {
	all, err := s.ListAll(ctx)
	if err != nil {
		return 0, err
	}

	cleaned := 0
	for _, rec := range all {
		nodeKey := redisKeyPrefix + "node:" + rec.NodeID
		exists, err := s.client.Exists(ctx, nodeKey).Result()
		if err != nil {
			continue
		}
		if exists == 0 {
			// Node is dead, clean up its sessions
			if err := s.Delete(ctx, rec.CallID); err == nil {
				cleaned++
				s.log.Infow("cleaned stale session", "callID", rec.CallID, "deadNode", rec.NodeID)
			}
		}
	}

	return cleaned, nil
}

// Close closes the Redis connection.
func (s *RedisStore) Close() error {
	return s.client.Close()
}

func (s *RedisStore) sessionKey(callID string) string {
	return redisKeyPrefix + "session:" + callID
}
