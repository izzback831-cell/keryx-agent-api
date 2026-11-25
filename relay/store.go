package main

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	recordsBucket = []byte("inference-requests")
	networkKey    = []byte("network")
	errFull       = errors.New("request registry is full")
)

const (
	acceptedRetention  = 48 * time.Hour
	uncertainRetention = time.Hour
)

type record struct {
	ID          string             `json:"transaction_id"`
	Cursor      string             `json:"cursor"`
	Created     time.Time          `json:"created_at"`
	Submitted   bool               `json:"submitted"`
	ConfirmedIn string             `json:"confirmed_in,omitempty"`
	Answers     []blockchainAnswer `json:"answers"`
	LastScan    time.Time          `json:"last_scan"`
}

func (r record) expired(now time.Time) bool {
	lifetime := uncertainRetention
	if r.Submitted || r.ConfirmedIn != "" || len(r.Answers) > 0 {
		lifetime = acceptedRetention
	}
	return !now.Before(r.Created.Add(lifetime))
}

type registry struct {
	db  *bolt.DB
	cap int
}

func openRegistry(path, network string, capacity int) (*registry, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(recordsBucket)
		if err != nil {
			return err
		}
		saved := bucket.Get(networkKey)
		if saved != nil && string(saved) != network {
			return errors.New("request registry belongs to a different Keryx network")
		}
		return bucket.Put(networkKey, []byte(network))
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &registry{db: db, cap: capacity}, nil
}

func (s *registry) get(id string, now time.Time) (*record, error) {
	var entry *record
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(recordsBucket).Get([]byte(id))
		if raw == nil {
			return nil
		}
		var saved record
		if err := json.Unmarshal(raw, &saved); err != nil {
			return err
		}
		if !saved.expired(now) {
			entry = &saved
		}
		return nil
	})
	return entry, err
}

func (s *registry) save(entry *record) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(recordsBucket)
		key := []byte(entry.ID)
		if bucket.Get(key) == nil && bucket.Stats().KeyN-1 >= s.cap {
			return errFull
		}
		return bucket.Put(key, raw)
	})
}

func (s *registry) discardRejected(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucket).Delete([]byte(id))
	})
}

func (s *registry) prune(now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(recordsBucket)
		var expired [][]byte
		if err := bucket.ForEach(func(key, data []byte) error {
			if string(key) == string(networkKey) {
				return nil
			}
			var entry record
			if err := json.Unmarshal(data, &entry); err != nil {
				return err
			}
			if entry.expired(now) {
				expired = append(expired, append([]byte(nil), key...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range expired {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}
