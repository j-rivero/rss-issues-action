/*
Copyright 2022 Guilhem Lettron (guilhem@barpilot.io).

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

        http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/akrennmair/slice"
	"github.com/google/go-github/v33/github"
	"golang.org/x/oauth2"

	funk "github.com/thoas/go-funk"

	"github.com/mmcdole/gofeed"

	gha "github.com/sethvargo/go-githubactions"

	md "github.com/JohannesKaufmann/html-to-markdown"
)

const (
	lastTimeInput             = "lastTime"
	labelsInput               = "labels"
	repoTokenInput            = "repo-token"
	feedInput                 = "feed"
	prefixInput               = "prefix"
	aggregateInput            = "aggregate"
	dryRunInput               = "dry-run"
	titleFilterInput          = "titleFilter"
	titleInclusionFilterInput = "titleInclusionFilter"
	contentFilterInput        = "contentFilter"
)

func main() {
	a := gha.New()
	a.AddPath("main.go")

	// Parse repository in form owner/name
	repo := strings.Split(os.Getenv("GITHUB_REPOSITORY"), "/")

	// Parse limit time option
	var limitTime time.Time
	if d, err := time.ParseDuration(a.GetInput(lastTimeInput)); err == nil {
		// Make duration negative
		if d > 0 {
			d = -d
		}
		limitTime = time.Now().Add(d)
	} else {
		a.Debugf("Fail to parse last time %s", a.GetInput(lastTimeInput))
	}
	a.Debugf("limitTime %s", limitTime)

	// Parse Labels
	labels := strings.Split(a.GetInput(labelsInput), ",")
	a.Debugf("labels %v", labels)

	ctx := context.Background()

	// Instanciate GitHub client
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: a.GetInput(repoTokenInput)},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// Instanciate feed parser
	fp := gofeed.NewParser()
	feed, err := fp.ParseURLWithContext(a.GetInput(feedInput), ctx)
	if err != nil {
		a.Errorf("Cannot parse feed '%s': '%s'", a.GetInput(feedInput), err)
		os.Exit(1)
	}
	a.Infof("%s", feed.Title)

	// Instanciate HTML to markdown
	converter := md.NewConverter("", true, nil)

	// Remove old items in feed
	feed.Items = funk.Filter(feed.Items, func(x *gofeed.Item) bool {
		if x.PublishedParsed != nil {
			return x.PublishedParsed.After(limitTime)
		}
		a.Infof("Item don't have a publish date, skip limitTime")
		return true
	}).([]*gofeed.Item)

	// Get all issues
	IssueListByRepoOption := &github.IssueListByRepoOptions{
		State:  "all",
		Labels: labels,
	}

	issues, _, err := client.Issues.ListByRepo(ctx, repo[0], repo[1], IssueListByRepoOption)
	if err != nil {
		a.Fatalf("%v", err)
	}
	a.Debugf("%d issues", len(issues))

	var issuesToCreate []*github.IssueRequest
	var createdIssues []*github.Issue

	// Iterate
	for _, item := range feed.Items {
		title := strings.Join([]string{a.GetInput(prefixInput), item.Title}, " ")
		a.Debugf("Issue '%s'", title)

		if issue := funk.Find(issues, func(x *github.Issue) bool {
			return *x.Title == title
		}); issue != nil {
			a.Warningf("Issue already exists")
			continue
		}

		// Issue Content

		content := item.Content
		if content == "" {
			content = item.Description
		}

		filter := a.GetInput(titleFilterInput)
		if filter != "" {
			matched, _ := regexp.MatchString(filter, item.Title)
			if matched {
				a.Debugf("No issue created due to title filter")
				continue
			}
		}
		filterInclusion := a.GetInput(titleInclusionFilterInput)
		a.Debugf("titleInclusionFilterInput: %s", filterInclusion)
		if filterInclusion != "" {
			matched, _ := regexp.MatchString(filterInclusion, item.Title)
			a.Debugf("titleInclusionFilterInput matched: %s", matched)
			if ! matched {
				a.Debugf("No issue created due to title inclusion filter")
				continue
			}
		}
		filter = a.GetInput(contentFilterInput)
		if filter != "" {
			matched, _ := regexp.MatchString(filter, content)
			if matched {
				a.Debugf("No issue created due to content filter")
				continue
			}
		}

		markdown, err := converter.ConvertString(content)
		if err != nil {
			a.Errorf("Fail to convert HTML to markdown: '%s'", err)
			continue
		}

		// truncate if characterLimit >0
		characterLimit := a.GetInput("characterLimit")
		if characterLimit != "" {
			cl, err := strconv.Atoi(characterLimit)
			if err != nil {
				a.Errorf("fail to convert 'characterLimit': '%s'", err)
				continue
			}
			if len(markdown) > cl {
				markdown = markdown[:cl] + "…"
				markdown += "\n\n---\n## Would you like to know more?\nRead the full article on the following website:"
			}
		}

		// Execute the template with a map as context
		context := map[string]string{
			"Link":    item.Link,
			"Content": markdown,
		}

		const issue = `
{{if .Content}}
{{ .Content }}
{{end}}
{{if .Link}}

<{{ .Link }}>
{{end}}
`
		var tpl bytes.Buffer
		if err := template.Must(template.New("issue").Parse(issue)).Execute(&tpl, context); err != nil {
			a.Warningf("Cannot render issue: '%s'", err)
			continue
		}

		body := tpl.String()

		// Default to creating an issue per item
		// Create first issue if aggregate
		if aggregate, err := strconv.ParseBool(a.GetInput(aggregateInput)); err != nil || !aggregate || len(issuesToCreate) == 0 {
			// Create Issue

			issueRequest := &github.IssueRequest{
				Title: &title,
				Body:  &body,
			}
			if len(labels) != 0 {
				issueRequest.Labels = &labels
			}
			issuesToCreate = append(issuesToCreate, issueRequest)
		} else {
			title = strings.Join([]string{a.GetInput(prefixInput), time.Now().Format(time.RFC822)}, " ")
			issuesToCreate[0].Title = &title

			body = fmt.Sprintf("%s\n\n%s", *issuesToCreate[0].Body, body)
			issuesToCreate[0].Body = &body
		}
	}

	for _, issueRequest := range issuesToCreate {
		if dr, err := strconv.ParseBool(a.GetInput(dryRunInput)); err != nil || !dr {

			issue, _, err := client.Issues.Create(ctx, repo[0], repo[1], issueRequest)
			if err != nil {
				a.Warningf("Fail create issue %s: %s", *issueRequest.Title, err)
				continue
			}
			createdIssues = append(createdIssues, issue)

		} else {
			a.Debugf("Creating Issue '%s' with content '%s'", *issueRequest.Title, *issueRequest.Body)
		}
	}

	createdIssuesString := slice.Map(createdIssues, func(ci *github.Issue) string { return strconv.Itoa(*ci.Number) })

	gha.SetOutput("issues", strings.Join(createdIssuesString, ","))
}
