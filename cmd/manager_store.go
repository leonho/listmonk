package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/gofrs/uuid/v5"
	"github.com/knadh/listmonk/internal/core"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/media"
	"github.com/knadh/listmonk/models"
	"github.com/lib/pq"
)

// store implements DataSource over the primary
// database.
type store struct {
	queries *models.Queries
	core    *core.Core
	media   media.Store
}

type runningCamp struct {
	CampaignID       int    `db:"campaign_id"`
	CampaignType     string `db:"campaign_type"`
	LastSubscriberID int    `db:"last_subscriber_id"`
	MaxSubscriberID  int    `db:"max_subscriber_id"`
	ListID           int    `db:"list_id"`
}

func newManagerStore(q *models.Queries, c *core.Core, m media.Store) *store {
	return &store{
		queries: q,
		core:    c,
		media:   m,
	}
}

// NextCampaigns retrieves active campaigns ready to be processed excluding
// campaigns that are also being processed. Additionally, it takes a map of campaignID:sentCount
// of campaigns that are being processed and updates them in the DB.
func (s *store) NextCampaigns(currentIDs []int64, sentCounts []int64) ([]*models.Campaign, error) {
	var out []*models.Campaign
	err := s.queries.NextCampaigns.Select(&out, pq.Int64Array(currentIDs), pq.Int64Array(sentCounts))
	return out, err
}

// NextSubscribers retrieves a subset of subscribers of a given campaign.
// Since batches are processed sequentially, the retrieval is ordered by ID,
// and every batch takes the last ID of the last batch and fetches the next
// batch above that.
func (s *store) NextSubscribers(campID, limit int) ([]models.Subscriber, error) {
	var camps []runningCamp
	if err := s.queries.GetRunningCampaign.Select(&camps, campID); err != nil {
		return nil, err
	}

	var listIDs []int
	for _, c := range camps {
		listIDs = append(listIDs, c.ListID)
	}

	if len(listIDs) == 0 {
		return nil, nil
	}

	var out []models.Subscriber
	err := s.queries.NextCampaignSubscribers.Select(&out, camps[0].CampaignID, camps[0].CampaignType, camps[0].LastSubscriberID, camps[0].MaxSubscriberID, pq.Array(listIDs), limit)
	if err != nil {
		return nil, err
	}

	// [FORK] Post-filter by subscriber attributes if campaign has _subscriber_filter.
	camp, err := s.GetCampaign(campID)
	if err != nil {
		log.Printf("[FORK-DEBUG] GetCampaign(%d) error: %v — skipping filter", campID, err)
		return out, nil // on error, skip filtering
	}
	attribsJSON, _ := json.Marshal(camp.Attribs)
	log.Printf("[FORK-DEBUG] campaign %d attribs=%s (nil=%v)", campID, string(attribsJSON), camp.Attribs == nil)
	beforeCount := len(out)
	out = filterSubscribersByAttribs(out, camp.Attribs)
	log.Printf("[FORK-DEBUG] campaign %d filter: %d -> %d subscribers", campID, beforeCount, len(out))

	return out, nil
}

// filterSubscribersByAttribs filters subscribers whose attribs contain all
// key-value pairs from the campaign's _subscriber_filter.
func filterSubscribersByAttribs(subs []models.Subscriber, campAttribs models.JSON) []models.Subscriber {
	if campAttribs == nil {
		return subs
	}
	filterRaw, ok := campAttribs["subscriberFilter"]
	if !ok {
		return subs
	}
	filter, ok := filterRaw.(map[string]any)
	if !ok || len(filter) == 0 {
		return subs
	}

	filtered := make([]models.Subscriber, 0, len(subs))
	for _, s := range subs {
		if matchAttribs(s.Attribs, filter) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// matchAttribs checks if subscriber attribs match all filter conditions.
//
// Supported filter syntax:
//
//	{"segment": "hot"}                → include only segment="hot"
//	{"segment": ["hot", "warm"]}      → include segment "hot" OR "warm"
//	{"!segment": ["cold", "cool"]}    → exclude segment "cold" or "cool"
//	{"!segment": "cold"}              → exclude segment "cold"
//
// All conditions are ANDed together. Subscribers missing a filtered key
// are excluded for include filters and included for exclude filters.
func matchAttribs(attribs models.JSON, filter map[string]any) bool {
	if attribs == nil {
		// No attribs: pass exclude-only filters, fail if any include filter exists.
		for k := range filter {
			if !strings.HasPrefix(k, "!") {
				return false
			}
		}
		return true
	}
	for k, v := range filter {
		negate := strings.HasPrefix(k, "!")
		key := k
		if negate {
			key = k[1:]
		}

		av, exists := attribs[key]
		avStr := fmt.Sprintf("%v", av)

		// Build list of values to match against.
		var vals []string
		switch vt := v.(type) {
		case []any:
			for _, item := range vt {
				vals = append(vals, fmt.Sprintf("%v", item))
			}
		default:
			vals = []string{fmt.Sprintf("%v", v)}
		}

		if negate {
			// Exclude: if attrib exists and matches any value, reject.
			if exists {
				for _, val := range vals {
					if avStr == val {
						return false
					}
				}
			}
		} else {
			// Include: attrib must exist and match one of the values.
			if !exists {
				return false
			}
			matched := false
			for _, val := range vals {
				if avStr == val {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

// GetCampaign fetches a campaign from the database.
func (s *store) GetCampaign(campID int) (*models.Campaign, error) {
	var out = &models.Campaign{}
	err := s.queries.GetCampaign.Get(out, campID, nil, nil, "default")
	return out, err
}

// UpdateCampaignStatus updates a campaign's status.
func (s *store) UpdateCampaignStatus(campID int, status string) error {
	_, err := s.queries.UpdateCampaignStatus.Exec(campID, status)
	return err
}

// UpdateCampaignCounts updates a campaign's status.
func (s *store) UpdateCampaignCounts(campID int, toSend int, sent int, lastSubID int) error {
	_, err := s.queries.UpdateCampaignCounts.Exec(campID, toSend, sent, lastSubID)
	return err
}

// GetAttachment fetches a media attachment blob.
func (s *store) GetAttachment(mediaID int) (models.Attachment, error) {
	m, err := s.core.GetMedia(mediaID, "", "", s.media)
	if err != nil {
		return models.Attachment{}, err
	}

	b, err := s.media.GetBlob(m.URL)
	if err != nil {
		return models.Attachment{}, err
	}

	return models.Attachment{
		Name:    m.Filename,
		Content: b,
		Header:  manager.MakeAttachmentHeader(m.Filename, "base64", m.ContentType),
	}, nil
}

// CreateLink registers a URL with a UUID for tracking clicks and returns the UUID.
func (s *store) CreateLink(url string) (string, error) {
	// Create a new UUID for the URL. If the URL already exists in the DB
	// the UUID in the database is returned.
	uu, err := uuid.NewV4()
	if err != nil {
		return "", err
	}

	var out string
	if err := s.queries.CreateLink.Get(&out, uu, url); err != nil {
		return "", err
	}

	return out, nil
}

// RecordBounce records a bounce event and returns the bounce count.
func (s *store) RecordBounce(b models.Bounce) (int64, int, error) {
	var res = struct {
		SubscriberID int64 `db:"subscriber_id"`
		Num          int   `db:"num"`
	}{}

	err := s.queries.UpdateCampaignStatus.Select(&res,
		b.SubscriberUUID,
		b.Email,
		b.CampaignUUID,
		b.Type,
		b.Source,
		b.Meta)

	return res.SubscriberID, res.Num, err
}

// BlocklistSubscriber blocklists a subscriber permanently.
func (s *store) BlocklistSubscriber(id int64) error {
	_, err := s.queries.BlocklistSubscribers.Exec(pq.Int64Array{id})
	return err
}

// DeleteSubscriber deletes a subscriber from the DB.
func (s *store) DeleteSubscriber(id int64) error {
	_, err := s.queries.DeleteSubscribers.Exec(pq.Int64Array{id})
	return err
}
