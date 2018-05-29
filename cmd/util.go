// Package cmd defines and implements command-line commands and flags
// used by fdio. Commands and flags are implemented using Cobra.
package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nareix/curl"
	toml "github.com/pelletier/go-toml"
	"github.com/retgits/fdio/database"
	"github.com/tomnomnom/linkheader"
	xmlpath "gopkg.in/xmlpath.v2"
)

const (
	githubRootEndpoint        string = "https://api.github.com"
	githubSearchEndpoint      string = "/search/code"
	githubActivitySearchQuery string = "sort=indexed&order=desc&q=filename%3Aactivity.json+flogo"
	githubTriggerSearchQuery  string = "sort=indexed&order=desc&q=filename%3Atrigger.json+flogo"
)

// executeRequest executes an HTTP request
func executeRequest(URL string, headers http.Header) (curl.Response, error) {
	// Prepare the cURL request
	req := curl.Get(URL)

	// Optionally add the HTTP headers
	if headers != nil {
		req.Headers = headers
	}

	// Set timeouts
	// DialTimeout is the TCP Connection Timeout
	// Timeout is the Download Timeout
	req.DialTimeout(time.Second * 10)
	req.Timeout(time.Second * 30)

	// Specify a progress monitor, otherwise it doesn't work
	req.Progress(func(p curl.ProgressStatus) {}, time.Second)

	// Execute the request and return the result
	res, err := req.Do()
	if err != nil {
		return res, err
	}

	// Return values
	return res, nil
}

// Crawl will search on GitHub for activity.json or trigger.json files that are related to Flogo
func Crawl(httpHeader http.Header, db *database.Database, timeout float64, contribType string) error {
	githubSearchQuery := ""
	if contribType == "Trigger" {
		githubSearchQuery = githubTriggerSearchQuery
	} else {
		githubSearchQuery = githubActivitySearchQuery
	}

	// Get first page of activities
	URL := fmt.Sprintf("%s%s?%s", githubRootEndpoint, githubSearchEndpoint, githubSearchQuery)
	log.Printf("Send request to %s", URL)
	response, err := executeRequest(URL, httpHeader)
	if err != nil {
		return err
	}

	// Unmarshal the JSON response
	var responseBody map[string]interface{}
	byteArray := []byte(response.Body)
	if err = json.Unmarshal(byteArray, &responseBody); err != nil {
		return err
	}

	// Collect the items in this set
	arrayMap, err := prepareItems(responseBody["items"].([]interface{}))
	if err != nil {
		return fmt.Errorf("error while converting response to array: %s", err.Error())
	}

	// Store them in the database
	db.InsertActs(arrayMap)
	if err != nil {
		return fmt.Errorf("error while loading data into the database: %s", err.Error())
	}

	if timeout != 0 {
		lastItem := arrayMap[len(arrayMap)-1]
		lastURL := lastItem["url"].(string)
		idx := strings.Index(lastURL, "/tree")
		update, err := checkLastUpdate(lastURL[:idx])
		if err != nil {
			return err
		}
		// If update is larger than timeout it means the last update to the last checked
		// repository was longer than the timeout we set. In that case we don't need to
		// scan any further
		if update > timeout {
			log.Printf("Maximum timeout reached. Last repo update was %v hours\n", update)
			return nil
		}
	}

	// Check how many pages exist
	var pages int
	links := linkheader.Parse(response.Headers.Get("Link"))
	for _, link := range links {
		if link.Rel == "last" {
			pages, _ = strconv.Atoi(link.URL[strings.Index(link.URL, "page=")+5:])
		}
	}

	log.Printf("Found a total number of %v pages\n", pages)

	for i := 2; i <= pages; i++ {
		// Execute the request
		URL := fmt.Sprintf("%s%s?%s&page=%v", githubRootEndpoint, githubSearchEndpoint, githubSearchQuery, i)
		log.Printf("Send request to %s", URL)
		response, err := executeRequest(URL, httpHeader)
		if err != nil {
			return err
		}

		// Unmarshal the JSON response
		var responseBody map[string]interface{}
		byteArray := []byte(response.Body)
		if err = json.Unmarshal(byteArray, &responseBody); err != nil {
			return err
		}

		// Collect the items in this set
		arrayMap, err := prepareItems(responseBody["items"].([]interface{}))
		if err != nil {
			return fmt.Errorf("error while converting response to array: %s", err.Error())
		}

		// Store them in the database
		db.InsertActs(arrayMap)
		if err != nil {
			return fmt.Errorf("error while loading data into the database: %s", err.Error())
		}

		if timeout != 0 {
			lastItem := arrayMap[len(arrayMap)-1]
			lastURL := lastItem["url"].(string)
			idx := strings.Index(lastURL, "/tree")
			update, err := checkLastUpdate(lastURL[:idx])
			if err != nil {
				return err
			}
			// If update is larger than timeout it means the last update to the last checked
			// repository was longer than the timeout we set. In that case we don't need to
			// scan any further
			if update > timeout {
				log.Printf("Maximum timeout reached. Last repo update was %v hours\n", update)
				return nil
			}
		}

		// Wait for 5 seconds so the GitHub search API limit won't be breached
		time.Sleep(5 * time.Second)
	}

	return nil
}

// checkLastUpdate sends an HTTP request to the HTML URL of a GitHub repository
// and retrieves the last commit date.
func checkLastUpdate(URL string) (float64, error) {
	// Execute the request and return the result
	res, err := executeRequest(URL, nil)
	if err != nil {
		return 0, err
	}

	reader := strings.NewReader(res.Body)
	xmlroot, xmlerr := xmlpath.ParseHTML(reader)

	if xmlerr != nil {
		return 0, err
	}

	var xpath string
	xpath = `//time-ago`
	path := xmlpath.MustCompile(xpath)
	if value, ok := path.String(xmlroot); ok {
		layout := "Jan 02, 2006"
		t, _ := time.Parse(layout, value)
		duration := time.Since(t)
		return duration.Hours(), nil
	}

	return 0, fmt.Errorf("could not find last update of %s", URL)
}

// prepareItems takes the raw response from the GitHub search API and turns it into an
// array of map[string]interface{} so it can be added to the database
func prepareItems(items []interface{}) ([]map[string]interface{}, error) {
	datamap := make([]map[string]interface{}, len(items))

	for idx, item := range items {
		// For each item we need some metadata
		// the full_name of the repository contains both the repository name as
		// well as the name of the owner
		project := item.(map[string]interface{})
		repository := project["repository"].(map[string]interface{})
		contentURL := strings.Replace(project["html_url"].(string), "github.com", "raw.githubusercontent.com", 1)
		contentURL = strings.Replace(contentURL, "/blob", "", 1)

		// Get the content of the actual file
		response, err := executeRequest(contentURL, nil)

		// Unmarshal the JSON response
		var responseBody map[string]interface{}
		byteArray := []byte(response.Body)
		if err = json.Unmarshal(byteArray, &responseBody); err != nil {
			log.Printf("error while calling %s: %s", contentURL, err.Error())
		}

		if err == nil && responseBody["type"] != nil {
			// Get the project path without activity.json at the end
			projectPath := project["path"].(string)
			if strings.Contains(projectPath, "activity.json") {
				projectPath = projectPath[:len(projectPath)-13]
			} else {
				projectPath = projectPath[:len(projectPath)-12]
			}

			// Get the project type
			projectType := responseBody["type"].(string)
			projectType = projectType[6:]
			projectType = strings.Replace(projectType, ":", "", -1)

			// Set author to unknown if it doesn't exist
			if responseBody["author"] == nil {
				responseBody["author"] = "Unknown"
			}

			// Set an empty string if the description doesn't exist
			if responseBody["description"] == nil {
				responseBody["description"] = ""
			}

			if responseBody["name"] != nil {
				tempMap := make(map[string]interface{})
				tempMap["name"] = responseBody["name"].(string)
				tempMap["type"] = projectType
				tempMap["description"] = responseBody["description"].(string)
				tempMap["url"] = fmt.Sprintf("https://github.com/%s/tree/master/%s", repository["full_name"].(string), projectPath)
				tempMap["uploadedon"] = ""
				tempMap["author"] = responseBody["author"].(string)
				tempMap["showcase"] = ""
				datamap[idx] = tempMap
				// For debug
				tempNameKey := strings.Replace(tempMap["name"].(string), " ", "", -1)
				tempNameKey = strings.ToLower(tempNameKey)
				tempKey := fmt.Sprintf("%s/%s", tempMap["author"].(string), tempNameKey)
				log.Printf("Added %s to the list", tempKey)
			}
		}
	}
	return datamap, nil
}

// TomlTreeToMap converts a toml tree to an array of map[string]interface{}. It does so
// by introspecting the tree and looking for the items that match a specific key.
func TomlTreeToMap(tree *toml.Tree, key string) ([]map[string]interface{}, error) {
	// Get the correct key
	queryResult := tree.Get(key)
	if queryResult == nil {
		return nil, fmt.Errorf("No items found in the tree")
	}

	// Prepare the result
	resultArray := queryResult.([]*toml.Tree)
	datamap := make([]map[string]interface{}, len(resultArray))
	for idx, val := range resultArray {
		datamap[idx] = val.ToMap()
	}
	return datamap, nil
}
