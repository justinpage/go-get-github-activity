package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peterhellberg/link"
)

type repo struct {
	Name     string    `json:"full_name"`
	PushedAt time.Time `json:"pushed_at"`
	Error    error
}

type stat struct {
	Total int
	Week  int64
}

type report struct {
	Name    string
	Summary int
	Error   error
}

func init() {
	log.SetFlags(0)
}

func main() {
	for _, org := range os.Args[1:] {
		if err := GetMostActivityInSixMonths(org); err != nil {
			log.Printf("Something went wrong: %v\n", err)
		}
	}
}

func GetMostActivityInSixMonths(org string) error {
	// 1. Get a list of all repos ordered by pushed_at
	log.Printf("Grabbing list of all repos for %s", org)

	client := &http.Client{}

	reposURL := "https://api.github.com/orgs/" + org + "/repos?sort=pushed"

	req, _ := http.NewRequest("GET", reposURL, nil)
	req.SetBasicAuth(os.Getenv("GITHUB_USERNAME"), os.Getenv("GITHUB_TOKEN"))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("getting index failed: %s", resp.Status)
	}

	var list []*repo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fmt.Errorf("unmarhaling index failed: %s", err)
	}

	var total int
	for _, l := range link.Parse(resp.Header.Get("link")) {
		if l.Rel == "last" {
			lastURL, err := url.Parse(l.String())
			if err != nil {
				return fmt.Errorf("list all repos by org failed: %s", err)
			}

			total, _ = strconv.Atoi(lastURL.Query().Get("page"))

		}
	}

	// Grab additional repos only if pagination is available
	if total > 0 {
		pendingRepoURLs := make(chan string)
		processedRepoURLs := make(chan []*repo, total-1) // have first item above

		// Create a max set of workers that match the amount of pages available
		for i := 2; i <= total; i++ {
			go workerForRepos(pendingRepoURLs, processedRepoURLs)
		}

		// Queue all available repos that we need to process
		for i := 2; i <= total; i++ {
			nextReposURL := reposURL + "&page=" + strconv.Itoa(i)
			pendingRepoURLs <- nextReposURL
		}

		// List will contain all repos ordered by pushed_at
		for i := 2; i <= total; i++ {
			list = append(list, <-processedRepoURLs...)
		}
	}

	// 2. Filter down list and keep anything pushed within the last six months
	log.Printf("Filtering list within six months of commit activity")

	now := time.Now().UTC()
	sixMonthsAgo := now.AddDate(0, -6, 0)

	filteredByPushDateRepos := filterRepos(list, func(item *repo) bool {
		return item.PushedAt.After(sixMonthsAgo)
	})

	// 3. Loop through each repo and get statistics for each project
	log.Printf("Getting statistics for each repo from list")

	pendingStatURLs := make(chan string)
	processedStatURLs := make(chan *report, len(filteredByPushDateRepos))

	// Create a max set of workers that match the first set of workers
	for i := 0; i < 50; i++ {
		go workerForStats(pendingStatURLs, processedStatURLs)
	}

	// Queue all available repos that we need stats for
	for _, v := range filteredByPushDateRepos {
		statsURL := "https://api.github.com/repos/"
		nextStatsURL := statsURL + v.Name + "/stats/commit_activity"

		pendingStatURLs <- nextStatsURL
	}

	var reportByStats []*report
	for i := 0; i < len(filteredByPushDateRepos); i++ {
		reportByStats = append(reportByStats, <-processedStatURLs)
	}

	// 4. Order report based on the number of commits over six months
	sort.Slice(reportByStats, func(i, j int) bool {
		return reportByStats[i].Summary < reportByStats[j].Summary
	})

	fmt.Println("\nSummary")
	fmt.Println("-------")

	pattern, _ := regexp.Compile(org + "/(\\.?[a-zA-Z0-9].+)/stats")
	for i := len(reportByStats) - 1; i >= 0; i-- {
		summary := reportByStats[i].Summary

		if summary > 0 {
			name := pattern.FindStringSubmatch(reportByStats[i].Name)[1]
			fmt.Printf("%s: %v\n", name, summary)
		}

	}

	return nil
}

func workerForRepos(
	pendingRepoURLs <-chan string, processedRepoURLs chan<- []*repo,
) {
	for pendingRepoURL := range pendingRepoURLs {
		processedRepoURLs <- fetchRepo(pendingRepoURL)
	}
}

func fetchRepo(url string) []*repo {
	client := &http.Client{}

	req, _ := http.NewRequest("GET", url, nil)
	req.SetBasicAuth(os.Getenv("GITHUB_USERNAME"), os.Getenv("GITHUB_TOKEN"))

	resp, err := client.Do(req)
	if err != nil {
		return []*repo{&repo{Error: err}}
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return []*repo{
			&repo{
				Error: fmt.Errorf(
					"fetching repo failed: %s for repo %s", resp.Status, url,
				),
			},
		}
	}

	var list []*repo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return []*repo{
			&repo{
				Error: fmt.Errorf(
					"unmarshaling repo failed: %s for repo %s", err, url,
				),
			},
		}
	}

	return list
}

func workerForStats(
	pendingStatURLs <-chan string, processedRepoURLs chan<- *report,
) {
	for pendingURL := range pendingStatURLs {
		processedRepoURLs <- fetchStat(pendingURL)
	}
}

func fetchStat(url string) *report {
	client := &http.Client{}

	req, _ := http.NewRequest("GET", url, nil)
	req.SetBasicAuth(os.Getenv("GITHUB_USERNAME"), os.Getenv("GITHUB_TOKEN"))

	// Grab statistic for repo within two minutes; use exponential back-off in
	// order to support Github's background job which fires when compiling these
	// statistics.
	//
	// Please see the following:
	// https://developer.github.com/v3/repos/statistics/#a-word-about-caching
	const timeout = 2 * time.Minute
	deadline := time.Now().Add(timeout)

	for tries := 0; time.Now().Before(deadline); tries++ {
		resp, err := client.Do(req)
		if err != nil {
			return &report{Error: err}
		}

		defer resp.Body.Close()

		// Statistics job has completed, send back the summarized results
		if resp.StatusCode == http.StatusOK {
			var stats []*stat
			if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
				return &report{
					Error: fmt.Errorf(
						"unmarshaling repo failed: %s for repo %s", err, url,
					),
				}
			}

			// Only keep statistics from the last six months
			now := time.Now().UTC()
			sixMonthsAgo := now.AddDate(0, -6, 0)

			filteredByWeekStats := filterStats(stats, func(item *stat) bool {
				week := time.Unix(item.Week, 0).UTC()
				return week.After(sixMonthsAgo)
			})

			var summary int
			for _, v := range filteredByWeekStats {
				summary += v.Total
			}

			return &report{strings.ToLower(url), summary, nil}
		}

		// Empty repository with no content found; default report
		if resp.StatusCode == http.StatusNoContent {
			return &report{url, 0, nil}
		}

		// Server refuses to authorize request; default report
		if resp.StatusCode == http.StatusForbidden {
			return &report{url, 0, nil}
		}

		// Statistics job has not completed, submit the request again
		log.Printf("(http %v); retrying request...", resp.StatusCode)
		time.Sleep(time.Second << uint(tries)) // exponential back-off
	}

	return &report{
		Error: fmt.Errorf(
			"server (%s) failed to respond after %s", url, timeout,
		),
	}
}

func filterRepos(list []*repo, f func(*repo) bool) []*repo {
	var bucket []*repo
	for _, v := range list {
		if f(v) {
			bucket = append(bucket, v)
		}
	}
	return bucket
}

func filterStats(list []*stat, f func(*stat) bool) []*stat {
	var bucket []*stat
	for _, v := range list {
		if f(v) {
			bucket = append(bucket, v)
		}
	}
	return bucket
}
