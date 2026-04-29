package services

/*
Assume this will be run synchronously by 1 process - NO LOCKING is done.
*/

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron"
)

type Snooze struct {
	ThreadId    string    `json:"threadId,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Description string    `json:"description,omitempty"`
	Once        bool      `json:"once,omitempty"`
	MessageId   string    `json:"messageId,omitempty"`
	Crontab     string    `json:"crontab,omitempty"`
	UnSnooze    time.Time `json:"unSnooze,omitempty"`
	Timezone    string    `json:"timezone,omitempty"` // IANA timezone or fixed offset from user's email
}

// LoadTimezone loads a *time.Location from an IANA name (e.g. "America/New_York")
// or a fixed UTC offset string like "-0500" or "+0530".
func LoadTimezone(tz string) (*time.Location, error) {
	// Try IANA first
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc, nil
	}
	// Try parsing as a fixed offset like "-0500" or "+0530"
	if len(tz) == 5 && (tz[0] == '+' || tz[0] == '-') {
		ref, err := time.Parse("-0700", tz)
		if err == nil {
			_, offset := ref.Zone()
			return time.FixedZone(tz, offset), nil
		}
	}
	return nil, fmt.Errorf("unknown timezone %q", tz)
}

// TimezoneFromTime extracts a timezone string from a time.Time value.
// Returns the IANA name if available, otherwise the fixed offset (e.g. "-0500").
func TimezoneFromTime(t time.Time) string {
	name, offset := t.Zone()
	// If the zone name looks like an IANA abbreviation (e.g. "EST", "PST"),
	// we can't reliably map it to an IANA name, so use the offset.
	// time.LoadLocation only works with full IANA names like "America/New_York".
	if _, err := time.LoadLocation(name); err == nil && name != "" && name != "Local" {
		return name
	}
	// Format as fixed offset
	h := offset / 3600
	m := (offset % 3600) / 60
	if m < 0 {
		m = -m
	}
	return fmt.Sprintf("%+03d%02d", h, m)
}

// snoozeFile is a JSON file that stores snooze entries.
// It is an array of Snooze objects.
// Overridden by InitDataPaths when running in a container.
var snoozeFile = "snooze.json"

// FindSnoozeByThread returns the snooze entry for a given thread root ID,
// or nil if none exists.
func FindSnoozeByThread(threadId string) (*Snooze, error) {
	snoozed, err := ReadSnoozeFile()
	if err != nil {
		return nil, err
	}
	for i, s := range snoozed {
		if s.ThreadId == threadId {
			return &snoozed[i], nil
		}
	}
	return nil, nil
}

func (s *Snooze) SaveSnooze(ctx context.Context) error {
	if s.ThreadId == "" {
		threadId, idErr := GetThreadRootId(ctx, s.MessageId)
		if idErr != nil {
			return fmt.Errorf("getting thread root id: %w", idErr)
		}
		s.ThreadId = threadId
	}

	// Delete any existing entry if it exists
	// NOTE: THIS MEANS THAT ONLY ONE SNOOZE ENTRY CAN EXIST PER MESSAGE THREAD
	delErr := s.DeleteSnooze()
	if delErr != nil {
		return fmt.Errorf("deleting existing snooze entry: %w", delErr)
	}

	// Re-read AFTER delete so we don't use a stale slice
	snoozed, err := ReadSnoozeFile()
	if err != nil {
		return err
	}

	// Add a new snooze entry
	snoozed = append(snoozed, *s)

	return SaveSnoozeFile(snoozed)
}

// DeleteSnooze deletes a snooze entry from the snoozeFile.
// If both MessageId and ThreadId are provided, only the entry with
// the matching MessageId is removed.
func (s *Snooze) DeleteSnooze() error {
	snoozed, err := ReadSnoozeFile()
	if err != nil {
		return err
	}

	// First try to remove by MessageId
	var filtered []Snooze
	for _, entry := range snoozed {
		if s.MessageId != "" && entry.MessageId == s.MessageId {
			continue
		}
		filtered = append(filtered, entry)
	}

	// If nothing was removed by MessageId, try by ThreadId
	if len(filtered) == len(snoozed) && s.ThreadId != "" {
		filtered = nil
		for _, entry := range snoozed {
			if entry.ThreadId == s.ThreadId {
				continue
			}
			filtered = append(filtered, entry)
		}
	}

	if len(filtered) == len(snoozed) {
		return nil
	}

	return SaveSnoozeFile(filtered)
}

func SaveSnoozeFile(snoozed []Snooze) error {
	if snoozed == nil {
		snoozed = []Snooze{}
	}
	data, err := json.MarshalIndent(snoozed, "", "  ")
	if err != nil {
		return err
	}

	// Write to a temp file then rename for atomic replacement.
	// This prevents a crash mid-write from corrupting snooze.json.
	tmp, err := os.CreateTemp(filepath.Dir(snoozeFile), ".snooze-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err = os.Rename(tmpName, snoozeFile); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

func ReadSnoozeFile() ([]Snooze, error) {
	data, err := os.ReadFile(snoozeFile)
	if err != nil {
		if os.IsNotExist(err) {
			return []Snooze{}, nil
		}
		return nil, err
	}
	var snoozed []Snooze
	if err := json.Unmarshal(data, &snoozed); err != nil {
		return nil, err
	}
	return snoozed, nil
}

// NextSnoozed returns the oldest expired (by UnSnooze) snooze entry
// without modifying the snooze file. The caller is responsible for
// calling AdvanceOrDeleteSnooze after successful execution.
func NextSnoozed() (*Snooze, error) {
	snoozed, err := ReadSnoozeFile()
	if err != nil {
		return nil, err
	}

	if len(snoozed) == 0 {
		return nil, nil
	}

	// Find the oldest expired entry
	now := time.Now()
	var oldest *Snooze
	for i, entry := range snoozed {
		if entry.UnSnooze.After(now) {
			continue
		}
		// UnSnooze time is before now, so this should be executed
		if oldest == nil || entry.UnSnooze.Before(oldest.UnSnooze) {
			oldest = &snoozed[i]
		}
	}

	if oldest == nil {
		return nil, nil
	}

	// Return a copy so the caller has a stable snapshot
	result := *oldest
	return &result, nil
}

// AdvanceOrDeleteSnooze should be called after a snooze has been
// successfully executed. One-shot entries are deleted; recurring
// entries have their UnSnooze advanced to the next cron tick.
func AdvanceOrDeleteSnooze(ctx context.Context, s *Snooze) error {
	if s.Once {
		return s.DeleteSnooze()
	}

	specParser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := specParser.Parse(s.Crontab)
	if err != nil {
		return fmt.Errorf("couldn't parse crontab expression: %w", err)
	}

	now := time.Now()
	if s.Timezone != "" {
		if loc, err2 := LoadTimezone(s.Timezone); err2 == nil {
			now = now.In(loc)
		}
	}
	s.UnSnooze = sched.Next(now)
	return s.SaveSnooze(ctx)
}
