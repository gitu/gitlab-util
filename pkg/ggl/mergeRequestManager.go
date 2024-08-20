package ggl

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cockroachdb/pebble"
	"github.com/xanzy/go-gitlab"
	"log"
	"os"
	"path"
	"reflect"
	"slices"
	"strconv"
	"time"
)

// MergeRequestManager is a struct that manages the merge requests
type MergeRequestManager struct {
	db               *pebble.DB
	gl               *gitlab.Client
	processQueue     chan mergeTarget
	AuthorUsername   *string
	ReviewerUsername *string
}

// NewMergeRequestManager creates a new MergeRequestManager
func NewMergeRequestManager(db *pebble.DB, gl *gitlab.Client) *MergeRequestManager {
	return &MergeRequestManager{db: db, gl: gl, processQueue: make(chan mergeTarget)}
}

func (m *MergeRequestManager) GetTimeStamp(timestampId string) (time.Time, error) {
	lastFetchData, closer, err := m.db.Get([]byte("ts-" + timestampId))
	if errors.Is(err, pebble.ErrNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	lastFetch, err := time.Parse(time.RFC3339, string(lastFetchData))
	if err != nil {
		return time.Time{}, err
	}
	err = closer.Close()
	return lastFetch, err
}

func (m *MergeRequestManager) setTimeStamp(timestampId string, t time.Time) error {
	return m.db.Set([]byte("ts-"+timestampId), []byte(t.Format(time.RFC3339)), pebble.Sync)
}

// GetOrFetchMergeRequests gets all the merge requests from the database or fetches them from the gitlab api
// if the last fetch was more than 1 minutes ago or if there are no merge requests in the database blocks until
// the merge requests are fetched
func (m *MergeRequestManager) GetOrFetchMergeRequests(force bool) ([]MergeRequestInfo, error) {
	err := m.FetchProjectsIfNotOutdated()
	if err != nil {
		log.Println("Error fetching projects", err)
		return nil, err
	}
	timestampId := fmt.Sprintf("last-fetch-mr-%s-%s", m.AuthorUsername, m.ReviewerUsername)
	lastFetch, err := m.GetTimeStamp(timestampId)
	if err != nil {
		log.Println("Error getting timestamp", err)
		return nil, err
	}
	if time.Since(lastFetch) > 1*time.Minute || force {
		err = m.FetchMergeRequests()
		if err != nil {
			log.Println("Error fetching merge requests", err)
			return nil, err
		}
		err = m.setTimeStamp(timestampId, time.Now())
		if err != nil {
			log.Println("Error setting timestamp", err)
			return nil, err
		}
	}
	return m.GetMergeRequests()
}

// FetchMergeRequests fetches the merge requests from the gitlab api
func (m *MergeRequestManager) FetchMergeRequests() error {
	if m.AuthorUsername == nil && m.ReviewerUsername == nil {
		return errors.New("author and/or reviewer username must be set")
	}
	opt := &gitlab.ListMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{
			Sort:    "desc",
			Page:    1,
			PerPage: 50,
		},
		AuthorUsername:   m.AuthorUsername,
		ReviewerUsername: m.ReviewerUsername,
		State:            gitlab.Ptr("opened"),
		Scope:            gitlab.Ptr("all"),
		Sort:             gitlab.Ptr("created_at"),
	}
	mrIds := make(map[string]bool)

	for {
		mrs, resp, err := m.gl.MergeRequests.ListMergeRequests(opt)
		if err != nil {
			return err
		}

		// Store the merge requests in the database
		for _, mr := range mrs {
			key := mrKey(mr.ID)
			mrIds[key] = true
			err = m.store(mrKey(mr.ID), mr)
			if err != nil {
				return err
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	// Delete merge requests that are no longer in the list
	iter, err := m.db.NewIter(prefixIterOptions([]byte("mr-")))
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		item := iter.Key()
		if !mrIds[string(item)] {
			err := m.db.Delete(item, pebble.Sync)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func keyUpperBound(b []byte) []byte {
	end := make([]byte, len(b))
	copy(end, b)
	for i := len(end) - 1; i >= 0; i-- {
		end[i] = end[i] + 1
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil // no upper-bound
}

func prefixIterOptions(prefix []byte) *pebble.IterOptions {
	return &pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: keyUpperBound(prefix),
	}
}

func (m *MergeRequestManager) FetchMergeRequest(id int) error {
	old, err := m.GetMergeRequest(id)
	if err != nil {
		return err
	}
	mr, _, err := m.gl.MergeRequests.GetMergeRequest(old.ProjectID, old.IID, &gitlab.GetMergeRequestsOptions{})
	if err != nil {
		return err
	}
	err = m.store(mrKey(mr.ID), mr)
	return err
}

// GetMergeRequest gets a merge request by id
func (m *MergeRequestManager) GetMergeRequest(id int) (*gitlab.MergeRequest, error) {
	var mr gitlab.MergeRequest
	err := m.load(mrKey(id), &mr)
	return &mr, err
}

type MergeRequestInfo struct {
	gitlab.MergeRequest
	Target mergeTarget
}

func (m *MergeRequestManager) GetMergeRequests() ([]MergeRequestInfo, error) {
	var mrs []gitlab.MergeRequest
	err := m.loadPrefix("mr-", &mrs)
	slices.SortFunc(mrs, func(a, b gitlab.MergeRequest) int {
		return b.UpdatedAt.Compare(*a.UpdatedAt)
	})
	mri := make([]MergeRequestInfo, len(mrs))
	for i, mr := range mrs {
		target := mergeTarget{}
		_ = m.load("merge-target-"+strconv.Itoa(mr.ID), &target)
		mri[i] = MergeRequestInfo{MergeRequest: mr, Target: target}
	}
	return mri, err
}

func mrKey(id int) string {
	return "mr-" + strconv.Itoa(id)
}

func (m *MergeRequestManager) FetchProjects() error {
	opts := gitlab.ListProjectsOptions{
		ListOptions: gitlab.ListOptions{
			PerPage: 20,
			Page:    1,
		},
	}
	for {
		projects, resp, err := m.gl.Projects.ListProjects(&opts)
		if err != nil {
			return err
		}

		// Store the projects in the database
		for _, project := range projects {
			err := m.store("project-"+strconv.Itoa(project.ID), project)
			if err != nil {
				return err
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return nil
}
func (m *MergeRequestManager) store(key string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.db.Set([]byte(key), data, pebble.Sync)
}

func (m *MergeRequestManager) loadPrefix(prefix string, v interface{}) error {
	// Ensure v is a pointer to a slice
	sliceValue := reflect.ValueOf(v)
	if sliceValue.Kind() != reflect.Ptr || sliceValue.Elem().Kind() != reflect.Slice {
		return errors.New("v must be a pointer to a slice")
	}

	sliceElemType := sliceValue.Elem().Type().Elem()

	iter, err := m.db.NewIter(prefixIterOptions([]byte(prefix)))
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		item, err := iter.ValueAndErr()
		if err != nil {
			return err
		}
		elem := reflect.New(sliceElemType).Interface()
		err = json.Unmarshal(item, elem)
		if err != nil {
			return err
		}
		sliceValue.Elem().Set(reflect.Append(sliceValue.Elem(), reflect.ValueOf(elem).Elem()))
	}
	return nil
}

func (m *MergeRequestManager) load(key string, v interface{}) error {
	data, closer, err := m.db.Get([]byte(key))
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, v)
	if err != nil {
		return err
	}
	return closer.Close()
}

func (m *MergeRequestManager) GetProject(id int) (*gitlab.Project, error) {
	var project gitlab.Project
	err := m.load("project-"+strconv.Itoa(id), &project)
	return &project, err
}

func (m *MergeRequestManager) GetProjects() ([]gitlab.Project, error) {
	var projects []gitlab.Project
	err := m.loadPrefix("project-", &projects)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(projects, func(a, b gitlab.Project) int {
		return cmp.Compare(a.NameWithNamespace, b.NameWithNamespace)
	})
	return projects, err
}

func (m *MergeRequestManager) PullDiff(id int) ([]*gitlab.MergeRequestDiff, error) {
	mr, err := m.GetMergeRequest(id)
	if err != nil {
		return nil, err
	}
	diff, _, err := m.gl.MergeRequests.ListMergeRequestDiffs(mr.ProjectID, mr.IID, &gitlab.ListMergeRequestDiffsOptions{
		Unidiff: gitlab.Ptr(true),
	})
	if err != nil {
		return nil, err
	}
	return diff, err
}

func (m *MergeRequestManager) FetchProjectsIfNotOutdated() error {

	lastFetch, err := m.GetTimeStamp("last-fetch-projects")

	if time.Since(lastFetch) > 60*time.Minute {
		err = m.FetchProjects()
		if err != nil {
			return err
		}
		err = m.setTimeStamp("last-fetch-projects", time.Now())
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *MergeRequestManager) ApproveAndMergeMergeRequest(id int, diff []*gitlab.MergeRequestDiff) interface{} {
	diffText := RenderDiffString(diff)

	mr, err := m.GetMergeRequest(id)
	if err != nil {
		return err
	}

	target := mergeTarget{
		Id:        mr.ID,
		ProjectID: mr.ProjectID,
		MergeID:   mr.IID,
		DiffHash:  diffText,
		Next:      time.Now(),
		Info:      "enabled",
		Active:    true,
	}
	return m.process(target)
}

func RenderDiffString(diff []*gitlab.MergeRequestDiff) string {
	diffText := ""
	for _, d := range diff {
		diffText += fmt.Sprintf("%v\n", d.Diff)
	}
	return diffText
}

type mergeTarget struct {
	Id        int
	ProjectID int
	MergeID   int
	DiffHash  string
	Info      string
	Latest    time.Time
	Next      time.Time
	Active    bool
}

func GetDefaultDb() (*pebble.DB, error) {
	tempDir := path.Join(os.TempDir(), "merge-request-manager")
	err := os.MkdirAll(tempDir, 0700)
	if err != nil {
		return nil, err
	}
	return pebble.Open(tempDir, nil)
}

func (m *MergeRequestManager) processMerge(target mergeTarget, mergeStatus string) {
	if !target.Active {
		log.Println("Target is not active - ", target.Id)
		return
	}
	target.Latest = time.Now()
	switch mergeStatus {
	case "approvals_syncing", "blocked_status", "checking", "ci_must_pass", "ci_still_running", "conflict",
		"external_status_checks", "jira_association_missing", "need_rebase", "unchecked", "locked_paths", "locked_lfs_files":
		m.reschedule(target, 1*time.Minute, "status "+mergeStatus+" - will check again in 1 minute")
		break
	case "not_approved":
		diff, err := m.PullDiff(target.Id)
		if err != nil {
			log.Println("Error pulling diff", err)
			m.reschedule(target, 1*time.Minute, "error pulling diff - will check again in 1 minute")
			break
		}
		currentDiff := RenderDiffString(diff)
		if currentDiff != target.DiffHash {
			log.Println("Diff changed ", target.Id)
			m.stopProcessing(target, "aborted - diff changed")
			break
		}

		mr, _, err := m.gl.MergeRequestApprovals.ApproveMergeRequest(target.ProjectID, target.MergeID, &gitlab.ApproveMergeRequestOptions{})
		if err != nil {
			log.Println("Error approving merge request", err)
			m.reschedule(target, 1*time.Minute, "error approving - will check again in 1 minute")
			break
		}
		log.Println("Approved merge request", mr.ID)
		err = m.store(mrKey(mr.ID), mr)
		if err != nil {
			log.Println("Error storing merge request", err)
		}
		m.reschedule(target, 0*time.Minute, "approved - will try to merge")
		break
	case "mergeable":
		mr, _, err := m.gl.MergeRequests.AcceptMergeRequest(target.ProjectID, target.MergeID, &gitlab.AcceptMergeRequestOptions{})
		if err != nil {
			log.Println("Error merging merge request", err)
			m.reschedule(target, 1*time.Minute, "error merging - will check again in 1 minute")
			break
		}
		log.Println(mr.Title, " new status ", mr.State, "-", mr.DetailedMergeStatus)
		err = m.store(mrKey(mr.ID), mr)
		if err != nil {
			log.Println("Error storing merge request", err)
		}
		m.stopProcessing(target, "merged")
		break
	case "discussions_not_resolved", "draft_status", "not_open", "requested_changes":
		m.stopProcessing(target, "aborted - "+mergeStatus)
		break
	default:
		log.Println("Unknown status", mergeStatus)
	}
}

func (m *MergeRequestManager) stopProcessing(target mergeTarget, info string) {
	log.Println("Stopping target", target.Id, "with info", info)
	target.Active = false
	target.Next = time.Now()
	target.Info = info
	m.storeTargetSilent(target)
}

func (m *MergeRequestManager) reschedule(target mergeTarget, delay time.Duration, info string) {
	log.Println("Rescheduling target", target.Id, "in", delay, "with info", info)
	target.Next = time.Now().Add(delay)
	target.Info = info
	m.storeTargetSilent(target)
}

func (m *MergeRequestManager) storeTargetSilent(target mergeTarget) {
	err := m.store("merge-target-"+strconv.Itoa(target.Id), target)
	if err != nil {
		log.Println("Error storing merge target", err)
	}
}

func (m *MergeRequestManager) process(target mergeTarget) error {
	err := m.store("merge-target-"+strconv.Itoa(target.Id), target)
	if err != nil {
		return err
	}
	m.processQueue <- target
	return nil
}

func (m *MergeRequestManager) processor() {
	log.Println("Starting processor")
	for target := range m.processQueue {
		mr, _, err := m.gl.MergeRequests.GetMergeRequest(target.ProjectID, target.MergeID, &gitlab.GetMergeRequestsOptions{})
		if err != nil {
			log.Println("Error fetching merge request for id", target.Id, err)
			continue
		}
		err = m.store(mrKey(mr.ID), mr)
		if err != nil {
			log.Println("Error storing merge request", err)
		}

		m.processMerge(target, mr.DetailedMergeStatus)
	}
}

func (m *MergeRequestManager) processEnqueuer() {
	log.Println("Starting enqueuer")
	for {
		var mrt []mergeTarget
		err := m.loadPrefix("merge-target-", &mrt)
		if err != nil {
			log.Println("Error loading merge targets", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, target := range mrt {
			if target.Active && target.Next.Before(time.Now()) {
				log.Println("Enqueuing target", target.Id)
				m.processQueue <- target
			}
			if !target.Active && target.Latest.Before(time.Now().Add(-30*time.Minute)) && target.Info != "aborted - diff changed" {
				log.Println("Deleting target", target.Id)
				err = m.db.Delete([]byte("merge-target-"+strconv.Itoa(target.Id)), pebble.Sync)
				if err != nil {
					log.Println("Error deleting target", target.Id, err)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func (m *MergeRequestManager) Start() *MergeRequestManager {
	go m.processor()
	go m.processEnqueuer()
	return m
}

func (m *MergeRequestManager) ClearMerge(id int) error {
	target := mergeTarget{
		Id:     id,
		Active: false,
		Next:   time.Now(),
		Latest: time.Now(),
		Info:   "cleared",
	}
	return m.store("merge-target-"+strconv.Itoa(id), target)
}

func (m *MergeRequestManager) Reviewer(reviewer string) *MergeRequestManager {
	if reviewer != "" {
		m.ReviewerUsername = &reviewer
	} else {
		m.ReviewerUsername = nil
	}
	return m
}

func (m *MergeRequestManager) Author(author string) *MergeRequestManager {
	if author != "" {
		m.AuthorUsername = &author
	} else {
		m.AuthorUsername = nil
	}
	return m
}
