package postoffice

import (
	"errors"
	"path/filepath"
	"time"
)

// DeliveriesVersion is the deliveries/<id>.json format written by the
// Dispatcher.
const DeliveriesVersion = 1

// Delivery states.
const (
	DeliveryPending = "pending" // not sent yet, or waiting for a retry
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"  // every attempt failed
	DeliverySkipped = "skipped" // the message closed before it could be sent
)

// Closing-note states of a sent delivery.
const (
	CloseSent     = "sent"     // the closing note (or Resolve) went out
	CloseAnswerer = "answerer" // this delivery's chat gave the answer; no note
	CloseFailed   = "failed"   // every attempt failed
)

// DeliveryRecord is deliveries/<id>.json: where the Dispatcher sent one
// operator message, and whether the other channels have been told it closed
// (§5.2). Only the Dispatcher writes it; the TUI reads it to show "sent to
// slack, signal".
type DeliveryRecord struct {
	Version    int        `json:"version"`
	ID         string     `json:"id"`
	Kind       Kind       `json:"kind"`
	Deliveries []Delivery `json:"deliveries"`
	// ClosedNotified is set once every delivery has had its closing note (or
	// none was due); the record is then final.
	ClosedNotified *time.Time `json:"closed_notified,omitempty"`
}

// Delivery is one route's send record.
type Delivery struct {
	Channel string `json:"channel"`
	Chat    string `json:"chat"`
	// MessageID is the transport's id for the (first piece of the) message;
	// closing notes reply to it.
	MessageID   string     `json:"message_id,omitempty"`
	Status      string     `json:"status"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	Attempts    int        `json:"attempts,omitempty"`
	NextAttempt time.Time  `json:"next_attempt,omitzero"`
	LastError   string     `json:"last_error,omitempty"`
	// Closed is the closing-note state (CloseSent, CloseAnswerer,
	// CloseFailed); empty while none has been sent.
	Closed        string `json:"closed,omitempty"`
	CloseAttempts int    `json:"close_attempts,omitempty"`
}

// DeliveredTo returns the channels the message was sent to, once each, in
// record order.
func (r DeliveryRecord) DeliveredTo() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range r.Deliveries {
		if d.Status == DeliverySent && !seen[d.Channel] {
			seen[d.Channel] = true
			out = append(out, d.Channel)
		}
	}
	return out
}

// Deliveries returns the Dispatcher's record for id, if it has one.
func (s *Store) Deliveries(id string) (DeliveryRecord, bool, error) {
	if err := checkID(id); err != nil {
		return DeliveryRecord{}, false, err
	}
	var r DeliveryRecord
	err := readJSON(s.DeliveryPath(id), &r)
	if errors.Is(err, ErrNotFound) {
		return DeliveryRecord{}, false, nil
	}
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	return r, true, nil
}

// putDeliveries writes a delivery record (write-then-rename). Dispatcher only.
func (s *Store) putDeliveries(r DeliveryRecord) error {
	if err := checkID(r.ID); err != nil {
		return err
	}
	data, err := marshal(r)
	if err != nil {
		return err
	}
	return s.writeAtomic(filepath.Join(s.Root, DirDeliveries, tmpDir), s.DeliveryPath(r.ID), data)
}
