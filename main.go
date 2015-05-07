package main

import (
	"bytes"
	"container/ring"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

var (
	ghAPIFl   = flag.String("github-api", "https://api.github.com", "Github API url")
	ghUserFl  = flag.String("user", "", "Github user name")
	ghPassFl  = flag.String("pass", "", "Github password")
	ghAuthKey = flag.String("auth-key", "", "Github auth key")
	ghOrgFl   = flag.String("organization", "optiopay", "Organization name as known on github")

	staleTimeFl = flag.Duration("stale", time.Hour*24, "Time after which person is assigned to pull request")
)

const botName = "optiopay-helper"

func init() {
	rand.Seed(time.Now().UnixNano())
}

type Repository struct {
	Name            string    `json:"name"`
	OpenIssuesCount int       `json:"open_issues_count"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// listRepos return list of repositories with at least one open issue
func listRepos() (repos []Repository, err error) {
	url := fmt.Sprintf("%s/orgs/%s/repos", *ghAPIFl, *ghOrgFl)
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

	repos = make([]Repository, 0)
	if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		return nil, fmt.Errorf("cannot decode response: %s", err)
	}
	return repos, nil
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type PullRequest struct {
	ID        int64           `json:"id"`
	Number    int64           `json:"number"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Assignee  *User           `json:"assignee"`
	URL       string          `json:"url"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	Head      PullRequestHead `json:"head"`
}

type PullRequestHead struct {
	Repo Repository `json:"repo"`
	User User       `json:"user"`
}

// stalePullRequests return all pull requests that do not have user assigned
// and were created more than staleTime ago.
func stalePullRequests(staleTime time.Duration) (stale []PullRequest, err error) {
	repos, err := listRepos()
	if err != nil {
		return nil, fmt.Errorf("cannot list repos: %s", err)
	}
	stale = make([]PullRequest, 0)
	for _, repo := range repos {
		if repo.OpenIssuesCount == 0 {
			continue
		}
		url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open", *ghAPIFl, *ghOrgFl, repo.Name)
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
		prs := make([]PullRequest, 0)
		if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
			return stale, fmt.Errorf("cannot decode response: %s", err)
		}
		now := time.Now()
		for _, pr := range prs {
			if pr.Assignee != nil {
				continue
			}
			if pr.CreatedAt.Add(staleTime).After(now) {
				continue
			}
			stale = append(stale, pr)
		}
	}
	return stale, nil
}

var (
	membersMu    sync.Mutex
	membersCache []User
)

// listMembers return all organization members.
// Globally cached.
func listMembers() (members []User, err error) {
	membersMu.Lock()
	defer membersMu.Unlock()

	if membersCache == nil {
		url := fmt.Sprintf("%s/orgs/%s/members", *ghAPIFl, *ghOrgFl)
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
		members := make([]User, 0)
		if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
			return nil, fmt.Errorf("cannot decode response: %s", err)
		}
		membersCache = members
		for _, member := range members {
			log.Printf("member found: %#v", member)
		}
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
		for _, member := range members {
			membersRing.Value = &member
			membersRing = membersRing.Next()
		}
	}

	member := membersRing.Value.(*User)
	membersRing = membersRing.Next()
	return *member, nil
}

func writeGithubComment(pullReq *PullRequest, comment string) error {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(map[string]interface{}{
		"body":        comment,
		"in_reply-to": pullReq.Number,
	})
	if err != nil {
		return fmt.Errorf("cannot JSON encode body: %s", err)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", *ghAPIFl, *ghOrgFl, pullReq.Head.Repo.Name, pullReq.Number)
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

// assignUser assign user to given pull request issue
func assignUser(pullReq *PullRequest, user *User) error {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(map[string]interface{}{
		"assignee": user.Login,
	})
	if err != nil {
		return fmt.Errorf("cannot encode body: %s", err)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d",
		*ghAPIFl, *ghOrgFl, pullReq.Head.Repo.Name, pullReq.Number)
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
	log.Printf("%s assigned to #%d issue of %q", user.Login, pullReq.Number, pullReq.Head.Repo.Name)
	comment := fmt.Sprintf("Pull request seem to be stale, assigning @%s to be responsive for making things done.", user.Login)
	if err := writeGithubComment(pullReq, comment); err != nil {
		log.Printf("cannot comment on %s's #%d pull request: %s", pullReq.Head.Repo.Name, pullReq.Number, err)
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

	var wg sync.WaitGroup
	for _, pr := range stale {
		wg.Add(1)

		go func(pullReq PullRequest) {
			defer wg.Done()

			// pick random user, but do not assing owner to handle his own pull
			// request
			var user User
			for {
				user, err = nextRandomMember()
				if user.ID != pullReq.Head.User.ID && user.Login != botName {
					break
				}
			}

			if err != nil {
				log.Fatalf("cannot pick user: %s", err)
			}
			if err := assignUser(&pullReq, &user); err != nil {
				log.Printf("cannot assign %q to %d: %s", user.Login, pullReq.ID, err)
			}
		}(pr)
	}
	wg.Wait()
}
