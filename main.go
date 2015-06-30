package main

import (
	"bytes"
	"container/ring"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ghAPIFl       = flag.String("github-api", "https://api.github.com", "Github API url")
	ghUserFl      = flag.String("user", "", "Github user name")
	ghPassFl      = flag.String("pass", "", "Github password")
	ghAuthKey     = flag.String("auth-key", "", "Github auth key")
	ghOrgFl       = flag.String("organization", "optiopay", "Organization name as known on github")
	ghTeamFl      = flag.String("team-id", "1070941", "The ID of the team that should get PRs assigned")
	slackURLFl    = flag.String("slack-url", "", "Slack Incomming WebHooks API URL")
	vacationUsers = flag.String("vacation", "", "Comma-separated list of devs on vacation. Format: $login:$startdate:$enddate, e.g. MikeRoetgers:2015-05-01:2015-05-12")

	staleTimeFl = flag.Duration("stale", time.Hour*24, "Time after which person is assigned to pull request")
	oldTimeFl   = flag.Duration("old", time.Hour*24*3, "Time after which pull request is notified on slack to work on pull request")

	repoRegex = regexp.MustCompile("https://github.com/(.+?)/(.+?)/.*")
	linkRegex = regexp.MustCompile(`.*<(.+?)>; rel="next".*`)
)

const botName = "optiopay-helper"

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type Issue struct {
	ID          int64        `json:"id"`
	Number      int64        `json:"number"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	User        *User        `json:"user"`
	Assignee    *User        `json:"assignee"`
	URL         string       `json:"url"`
	HTMLURL     string       `json:"html_url"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	PullRequest *PullRequest `json:"pull_request"`
}

type PullRequest struct {
	HTMLURL string `json:"html_url"`
}

func (i *Issue) GetRepository() (string, error) {
	list := repoRegex.FindStringSubmatch(i.HTMLURL)
	if len(list) != 3 {
		return "", errors.New("URL has unexpected format")
	}
	return list[2], nil
}

func (i *Issue) isPullRequest() bool {
	return i.PullRequest != nil
}

// stalePullRequests return all pull requests that were created more than
// staleTime ago.
func stalePullRequests(staleTime time.Duration) (stale []Issue, err error) {
	stale = make([]Issue, 0)

	url := fmt.Sprintf("%s/orgs/%s/issues?filter=all&state=open", *ghAPIFl, *ghOrgFl)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create GET request: %s", err)
	}
	addAuthentication(req)

	loadIssues := func(r *http.Request) ([]Issue, string, error) {
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			return nil, "", fmt.Errorf("cannot fetch response: %s", err)
		}
		defer resp.Body.Close()
		var issues []Issue
		if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
			return nil, "", fmt.Errorf("cannot decode response: %s", err)
		}
		list := linkRegex.FindStringSubmatch(resp.Header.Get("Link"))
		nextURL := ""
		if len(list) == 2 {
			nextURL = list[1]
		}
		return issues, nextURL, nil
	}

	var issues []Issue
	loadMore := true
	for loadMore == true {
		newIssues, nextURL, loadErr := loadIssues(req)
		if loadErr != nil {
			panic("Failed to load: " + loadErr.Error())
		}
		issues = append(issues, newIssues...)
		if nextURL == "" {
			loadMore = false
		} else {
			req, err = http.NewRequest("GET", nextURL, nil)
			if err != nil {
				return nil, fmt.Errorf("cannot create GET request: %s", err)
			}
			addAuthentication(req)
		}
	}

	now := time.Now()
	for _, issue := range issues {
		if !issue.isPullRequest() {
			continue
		}
		if issue.CreatedAt.Add(staleTime).After(now) {
			continue
		}
		stale = append(stale, issue)
	}
	return stale, nil
}

type VacationUsers []string

func (u VacationUsers) Contains(entry string) bool {
	for _, user := range u {
		if user == entry {
			return true
		}
	}
	return false
}

var (
	membersMu    sync.Mutex
	membersCache []User
)

// listMembers return all members of a given team (configured by flag).
// Globally cached.
func listMembers() (members []User, err error) {
	membersMu.Lock()
	defer membersMu.Unlock()

	if membersCache == nil {
		url := fmt.Sprintf("%s/teams/%s/members", *ghAPIFl, *ghTeamFl)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot create GET request: %s", err)
		}
		addAuthentication(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cannot fetch response: %s", err)
		}
		defer resp.Body.Close()
		var members []User
		if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
			return nil, fmt.Errorf("cannot decode response: %s", err)
		}
		var onVacation VacationUsers
		now := time.Now()
		records := strings.Split(*vacationUsers, ",")
		for _, record := range records {
			parts := strings.Split(record, ":")
			if len(parts) != 3 {
				continue
			}
			from, fromErr := time.Parse("2006-01-02", parts[1])
			if fromErr != nil {
				continue
			}
			to, toErr := time.Parse("2006-01-02", parts[2])
			if toErr != nil {
				continue
			}
			to = to.Add(24 * time.Hour)
			if now.After(from) && now.Before(to) {
				onVacation = append(onVacation, parts[0])
			}
		}
		if len(onVacation) > 0 {
			for key, user := range members {
				if onVacation.Contains(user.Login) {
					members = append(members[:key], members[key+1:]...)
				}
			}
		}
		membersCache = members
	}
	return membersCache, nil
}

var (
	membersRMu  sync.Mutex
	membersRing *ring.Ring
)

// nextRandomMember returns random member, selected from round robin of all
// members.
//
// Because assigning randomly may not always produce best result, use round
// robin of random order members to get assignment user.
func nextRandomMember() (User, error) {
	membersRMu.Lock()
	defer membersRMu.Unlock()

	if membersRing == nil {
		members, err := listMembers()
		if err != nil {
			return User{}, fmt.Errorf("cannot list memebers: %s", err)
		}
		membersRing = ring.New(len(members))
		for key := range members {
			membersRing.Value = &members[key]
			membersRing = membersRing.Next()
		}

		// skip random number of users, to not always start from the same place
		skip, _ := rand.Int(rand.Reader, big.NewInt(int64(len(members))))
		for i := int64(0); i < skip.Int64(); i++ {
			membersRing = membersRing.Next()
		}
	}

	member := membersRing.Value.(*User)
	membersRing = membersRing.Next()
	return *member, nil
}

func writeGithubComment(issue *Issue, comment string) error {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(map[string]interface{}{
		"body":        comment,
		"in_reply-to": issue.Number,
	})
	if err != nil {
		return fmt.Errorf("cannot JSON encode body: %s", err)
	}
	repo, repoErr := issue.GetRepository()
	if repoErr != nil {
		return fmt.Errorf("Cannot extract repo name from URL: %s", repoErr)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", *ghAPIFl, *ghOrgFl, repo, issue.Number)
	req, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return fmt.Errorf("cannot create POST request: %s", err)
	}
	addAuthentication(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot do request: %s", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected response: %d", resp.StatusCode)
	}
	return nil
}

func remindOnSlack(issue *Issue) error {
	if *slackURLFl == "" {
		return errors.New("not supported")
	}
	log.Printf("Reminding %s to work on PR #%d (%s)\n", issue.Assignee.Login, issue.Number, issue.Title)
	// github login doesn't have to be slack login as well...
	msg := map[string]interface{}{
		"username":   "github-pr",
		"icon_emoji": ":octocat:",
		"text": fmt.Sprintf(`@%s, please work on <%s|Pull Request #%d> (%s)`,
			issue.Assignee.Login, issue.HTMLURL, issue.Number, issue.Title),
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("cannot JSON encode data: %s", err)
	}
	resp, err := http.Post(*slackURLFl, "application/json", bytes.NewBuffer(b))
	if err != nil {
		return fmt.Errorf("cannot POST data: %s", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("invalid response: %d, %s", resp.StatusCode, body)
	}
	return nil
}

// assignUser assign user to given pull request issue
func assignUser(issue *Issue, user *User) error {
	repo, repoErr := issue.GetRepository()
	if repoErr != nil {
		return fmt.Errorf("Cannot extract repo name from URL: %s", repoErr)
	}
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(map[string]interface{}{
		"assignee": user.Login,
	})
	if err != nil {
		return fmt.Errorf("cannot encode body: %s", err)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d",
		*ghAPIFl, *ghOrgFl, repo, issue.Number)
	req, err := http.NewRequest("PATCH", url, &body)
	if err != nil {
		return fmt.Errorf("cannot create PATCH request: %s", err)
	}
	addAuthentication(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot do request: %s", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected response: %d", resp.StatusCode)
	}
	log.Printf("%s assigned to #%d issue of %q", user.Login, issue.Number, repo)
	comment := fmt.Sprintf("Pull request seem to be stale, assigning @%s as the responsible developer.", user.Login)
	if err := writeGithubComment(issue, comment); err != nil {
		log.Printf("cannot comment on %s's #%d pull request: %s", repo, issue.Number, err)
	}
	return nil
}

// addAuthentication adds to given HTTP request authentication credentials
func addAuthentication(req *http.Request) {
	if *ghAuthKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("token %s", *ghAuthKey))
	} else {
		req.SetBasicAuth(*ghUserFl, *ghPassFl)
	}
}

func main() {
	flag.Parse()

	stale, err := stalePullRequests(*staleTimeFl)
	if err != nil {
		log.Fatalf("cannot fetch stale pull requests: %s", err)
	}

	now := time.Now()

	var wg sync.WaitGroup
	for _, pr := range stale {
		wg.Add(1)

		go func(issue Issue) {
			defer wg.Done()

			if issue.Assignee == nil {
				// pick random user, but do not assing owner to handle his own pull
				// request
				var user User
				for {
					user, err = nextRandomMember()
					if user.ID != issue.User.ID && user.Login != botName {
						break
					}
				}

				if err != nil {
					log.Fatalf("cannot pick user: %s", err)
				}
				if err := assignUser(&issue, &user); err != nil {
					log.Printf("cannot assign %q to %d: %s", user.Login, issue.ID, err)
				}
				return
			}

			if *slackURLFl != "" && issue.CreatedAt.Add(*oldTimeFl).Before(now) {
				if err := remindOnSlack(&issue); err != nil {
					log.Printf("cannot write slack notification: %s", err)
				}
			}

		}(pr)
	}
	wg.Wait()
}
