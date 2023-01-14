package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tensai75/nntp"
	"github.com/Tensai75/nzb-monkey-go/nzbparser"
	"github.com/Tensai75/nzb-monkey-go/subjectparser"
	progressbar "github.com/schollz/progressbar/v3"
)

var directsearchHits = make(map[string]map[string]nzbparser.NzbFile)
var directsearchCounter int

func nzbdirectsearch(engine SearchEngine) (*nzbparser.Nzb, error) {

	if conf.Directsearch.Username == "" || conf.Directsearch.Password == "" {
		return nil, fmt.Errorf("No or incomplete credentials for usenet server")
	}
	if len(args.Groups) == 0 {
		return nil, fmt.Errorf("No groups provided")
	}
	if args.UnixDate == 0 {
		return nil, fmt.Errorf("No date provided")
	}
	if conf.Directsearch.Connections == 0 {
		conf.Directsearch.Connections = 20
	}
	if conf.Directsearch.Days == 0 {
		conf.Directsearch.Days = 2
	}
	if conf.Directsearch.Scans == 0 {
		conf.Directsearch.Scans = 50
	}
	if conf.Directsearch.Step == 0 {
		conf.Directsearch.Step = 20000
	}

	for _, group := range args.Groups {
		fmt.Println()
		Log.Info("Searching in group '%s' ...", group)
		if err := searchInGroup(group); err != nil {
			Log.Error(err.Error())
		} else {
			if len(directsearchHits) > 0 {
				for _, hit := range directsearchHits {
					var nzb = &nzbparser.Nzb{}
					for _, files := range hit {
						nzb.Files = append(nzb.Files, files)
					}
					nzbparser.MakeUnique(nzb)
					nzbparser.ScanNzbFile(nzb)
					sort.Sort(nzb.Files)
					for id := range nzb.Files {
						sort.Sort(nzb.Files[id].Segments)
					}
					return nzb, nil
				}
			}
			Log.Warn("No result in group '%s'", group)
		}
	}
	return nil, fmt.Errorf("No results found")

}

func searchInGroup(group string) error {
	var searchesWG sync.WaitGroup
	var searchesGuard = make(chan struct{}, conf.Directsearch.Scans)
	defer close(searchesGuard)
	var searchesErrorChannel = make(chan error)
	defer close(searchesErrorChannel)
	var searchesCtx, searchesCancel = context.WithCancel(context.Background())
	defer searchesCancel() // Make sure it's called to release resources even if no errors
	var step = conf.Directsearch.Step
	var currentMessageID int
	conn, firstMessageID, lastMessageID, err := switchToGroup(group)
	if err != nil {
		return err
	}
	lastMessageID, _, err = scanForDate(conn, firstMessageID, lastMessageID, 0, false, "   Scanning for start date ...")
	if err != nil {
		DisconnectNNTP(conn)
		return fmt.Errorf("Error while scanning group '%s' for the last message: %v\n", group, err)
	}
	currentMessageID, _, err = scanForDate(conn, firstMessageID, lastMessageID, -1*conf.Directsearch.Days*60*60*24, true, "   Scanning for end date ...  ")
	if err != nil {
		DisconnectNNTP(conn)
		return fmt.Errorf("Error while scanning group '%s' for the first message: %v\n", group, err)
	}
	if currentMessageID >= lastMessageID {
		DisconnectNNTP(conn)
		return errors.New("no messages found within search range")
	}
	DisconnectNNTP(conn)
	bar := progressbar.NewOptions(lastMessageID-currentMessageID,
		progressbar.OptionSetDescription("   Scanning messages ...      "),
		progressbar.OptionSetRenderBlankState(true),
	)
	for currentMessageID <= lastMessageID {
		var lastMessage int
		if currentMessageID+step > lastMessageID {
			lastMessage = lastMessageID
		} else {
			lastMessage = currentMessageID + step
		}
		go func(ctx context.Context, currentMessageID int, lastMessage int, group string, bar *progressbar.ProgressBar) {
			searchesWG.Add(1)
			defer searchesWG.Done()
			searchesGuard <- struct{}{} // will block if guard channel is already filled
			defer func() { <-searchesGuard }()
			if err := searchMessages(ctx, currentMessageID, lastMessage, group, bar); err != nil {
				searchesErrorChannel <- err
				searchesCancel()
			}
		}(searchesCtx, currentMessageID, lastMessage, group, bar)
		// update currentMessageID for next request
		currentMessageID = lastMessage + 1
	}
	barRunner := true
	go func(barRunner *bool) {
		searchesWG.Wait()
		*barRunner = false
	}(&barRunner)
	for barRunner {
		time.Sleep(100 * time.Millisecond)
		bar.Set(directsearchCounter)
	}
	bar.Finish()
	fmt.Println()
	select {
	case <-searchesErrorChannel:
		return err
	default:
		return nil
	}
}

func searchMessages(ctx context.Context, firstMessage int, lastMessage int, group string, bar *progressbar.ProgressBar) error {
	select {
	case <-ctx.Done():
		return nil // Error somewhere, terminate
	default: // required, otherwise it will block
	}
	conn, firstMessageID, lastMessageID, err := switchToGroup(group)
	defer DisconnectNNTP(conn)
	if err != nil {
		return err
	}
	if firstMessage < firstMessageID {
		firstMessage = firstMessageID
	}
	if lastMessage > lastMessageID {
		lastMessage = lastMessageID
	}
	select {
	case <-ctx.Done():
		return nil // Error somewhere, terminate
	default: // required, otherwise it will block
	}
	results, err := conn.Overview(firstMessage, lastMessage)
	DisconnectNNTP(conn)
	if err != nil {
		return fmt.Errorf("Error retrieving message overview from the usenet server while searching in group '%s': %v\n", group, err)
	}
	for _, overview := range results {
		select {
		case <-ctx.Done():
			return nil // Error somewhere, terminate
		default: // required, otherwise it will block
		}
		currentDate := overview.Date.Unix()
		if currentDate >= args.UnixDate {
			return nil
		}
		subject := html.UnescapeString(strings.ToValidUTF8(overview.Subject, ""))
		searchPattern := regexp.MustCompile("(?i)" + regexp.QuoteMeta(args.Header))
		if match := searchPattern.Match([]byte(subject)); match {
			if subject, err := subjectparser.Parse(subject); err == nil {
				var date int64
				if date = overview.Date.Unix(); date < 0 {
					date = 0
				}
				poster := strings.ToValidUTF8(overview.From, "")
				// make hashes
				headerHash := GetMD5Hash(subject.Header + poster + strconv.Itoa(subject.TotalFiles))
				fileHash := GetMD5Hash(headerHash + subject.Filename + strconv.Itoa(subject.TotalSegments))
				if _, ok := directsearchHits[headerHash]; !ok {
					directsearchHits[headerHash] = make(map[string]nzbparser.NzbFile)
				}
				if _, ok := directsearchHits[headerHash][fileHash]; !ok {
					directsearchHits[headerHash][fileHash] = nzbparser.NzbFile{
						Groups:       []string{group},
						Subject:      subject.Subject,
						Poster:       poster,
						Number:       subject.File,
						Filename:     subject.Filename,
						Basefilename: subject.Basefilename,
					}
				}
				file := directsearchHits[headerHash][fileHash]
				if file.Groups[len(file.Groups)-1] != group {
					file.Groups = append(file.Groups, group)
				}
				if subject.Segment == 1 {
					file.Subject = subject.Subject
				}
				if int(date) > file.Date {
					file.Date = int(date)
				}
				file.Segments = append(file.Segments, nzbparser.NzbSegment{
					Number: subject.Segment,
					Id:     strings.Trim(overview.MessageId, "<>"),
					Bytes:  overview.Bytes,
				})
				directsearchHits[headerHash][fileHash] = file
			}
		}
		directsearchCounter += 1
	}
	return nil
}

func scanForDate(conn *nntp.Conn, firstMessageID int, lastMessageID int, interval int, first bool, text string) (int, time.Time, error) {
	bar := progressbar.NewOptions(lastMessageID-firstMessageID,
		progressbar.OptionSetDescription(text),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionThrottle(time.Millisecond*100),
	)
	defer func() {
		bar.Finish()
		fmt.Println()
	}()
	currentMessageID := firstMessageID
	endMessageID := lastMessageID
	scanStep := endMessageID - currentMessageID
	for currentMessageID <= endMessageID {
		step := 0
		if currentMessageID == firstMessageID {
			step = 2000
		}
		if scanStep < 1000 {
			results, err := conn.Overview(currentMessageID-1000, currentMessageID+1000)
			if err != nil {
				return 0, time.Time{}, err
			}
			for _, overview := range results {
				bar.Add(1)
				if overview.Date.Unix() > args.UnixDate+int64(interval) {
					return overview.MessageNumber, overview.Date, nil
				}
			}
			return results[len(results)-1].MessageNumber, results[len(results)-1].Date, nil
		} else {
			results, err := conn.Overview(currentMessageID, currentMessageID+step)
			if err != nil {
				return 0, time.Time{}, err
			}
			if len(results) == 0 {
				return 0, time.Time{}, fmt.Errorf("Overview results are empty")
			}
			overview := results[0]
			currentDate := overview.Date.Unix()
			scanStep = scanStep / 2
			if first && currentMessageID == firstMessageID && currentDate > args.UnixDate+int64(interval) {
				return overview.MessageNumber, overview.Date, nil
			} else if !first && currentMessageID == firstMessageID && currentDate > args.UnixDate+int64(interval) {
				return 0, time.Time{}, fmt.Errorf("post date is older than oldest message of this group")
			}
			if currentDate < args.UnixDate+int64(interval) {
				currentMessageID = currentMessageID + scanStep
			}
			if currentDate > args.UnixDate+int64(interval) {
				currentMessageID = currentMessageID - scanStep
			}
		}
		bar.Add(scanStep)

	}
	return 0, time.Time{}, fmt.Errorf("no messages found within search range")
}

func switchToGroup(group string) (*nntp.Conn, int, int, error) {
	conn, err := ConnectNNTP()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("Error connecting to the usenet server: %v", err)
	}
	_, firstMessageID, lastMessageID, err := conn.Group(group)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("Error retrieving group information for group '%s' from the usenet server: %v\n", group, err)
	}
	return conn, firstMessageID, lastMessageID, nil
}

func GetMD5Hash(text string) string {
	hasher := md5.New()
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}
