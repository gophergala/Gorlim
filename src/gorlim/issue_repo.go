package gorlim

import "github.com/libgit2/git2go"
import "strconv"
import "os"

import "bytes"
import "strings"
import "bufio"

import "os/exec"
import "time"
import "sync"
//import "path/filepath"

import "fmt"

// threshold for number of commits to repo
// before issuing next "garbage collection"
const gcThreshold = 2048

type issueRepository struct {
	path string
	gcCounter int
	repo *git.Repository
	mutex *sync.Mutex
	prePushHook PrePushHook
}

var mutex = &sync.Mutex{}
var id int = 0
var listener PrePushListener = CreatePrePushListener()
var repoMap map[string]*issueRepository = make(map[string]*issueRepository)

func init() {
	// listen to pre-push events
	go func() {
		for {
			replyChannel := listener.GetReplyChannel()
			for prePush := range listener.GetPrePushChannel() {
				irepo := repoMap[prePush.RepoPath]
				if irepo.prePushHook == nil {
					replyChannel <- RepoPrePushReply { Status : true }
					continue
				}
				irepo.establishExclusiveRepoConnection()
				oid, err := git.NewOid(prePush.Sha)
				if err != nil {
					irepo.closeExclusiveRepoConnection()
					replyChannel <- RepoPrePushReply { Status: false, Err : "invalid commit SHA" }
					continue
				}
				head, _ := irepo.repo.Head()
				diff := irepo.diff(head.Target(), oid)
				irepo.closeExclusiveRepoConnection()
				if err = irepo.prePushHook(diff); err != nil {
					replyChannel <- RepoPrePushReply { Status: false, Err : err.Error() }
				} else {
					replyChannel <- RepoPrePushReply { Status : true }
				}
			}
		}
	}()
}

func (r *issueRepository) SetPrePushHook(pph PrePushHook) {
	r.prePushHook = pph
}

func (r *issueRepository) lock() {
	r.mutex.Lock()
}

func (r *issueRepository) unlock() {
	r.mutex.Unlock()
}

func (r *issueRepository) initialize(repoPath string) {
	r.path = repoPath
	r.gcCounter = 0
	r.mutex = &sync.Mutex{}
	// create physical repo
	repo, err := git.InitRepository(r.path, false)
	if err != nil {
		panic("Failed to create repo with path " + repoPath)
	}
	repo.Free()
	// configure
	setIgnoreDenyCurrentBranch(r.path) // allow push to non-bare repo
	// setup pre-receive hook
	pre, err := os.Create(r.path + "/.git/hooks/pre-receive")
	if err != nil {
		panic(err)
	}	
	defer pre.Close()
	pre.Chmod(0777)
	pre.WriteString("#!/bin/sh\n")
	pre.WriteString("read oldrev newrev refname\n")
	pre.WriteString("$(HOME)/pre_push_hook " + repoPath + " $oldrev $newrev\n")
	// setup post-receive hook
	/*post, err := os.Create(r.path + "/.git/hooks/post-receive")
	if err != nil {
		panic(err)
	}
	defer post.Close()
	post.Chmod(0777)
	post.WriteString("#!/bin/sh\n")
	post.WriteString("echo " + strconv.Itoa(r.id) + " >" + getPushPipeName())*/
	return
}

func blobToIssue(blob []byte) []string {
	// Well, that is potentially wrong because of different encodings
	str := string(blob[:len(blob)])
	return strings.Split(str, "\n")
}

func (r *issueRepository) diff(oldOid *git.Oid, newOid *git.Oid) CommitDiff {
	repo := r.repo

	// find commits
	c1, err := repo.LookupCommit(oldOid)
	if err != nil {
		panic("Failed to find commit by oid")
	}
	c2, err := repo.LookupCommit(newOid)
	if err != nil {
		panic("Failed to find commit by oid")
	}
	// get commit trees
	t1, err := c1.Tree()
	if err != nil {
		panic(err)
	}
	t2, err := c2.Tree()
	if err != nil {
		panic(err)
	}
	// get diff
	diffOpts, _ := git.DefaultDiffOptions()
	diff, err := repo.DiffTreeToTree(t1, t2, &diffOpts)
	if err != nil {
		panic(err)
	}
	parseIssue := func(file git.DiffFile) Issue {
		blob, _ := repo.LookupBlob(file.Oid)
		issue := Issue{}
		parseIssuePropertiesFromRepoPath(file.Path, &issue)
		parseIssuePropertiesFromText(blobToIssue(blob.Contents()), &issue)
		return issue
	}
	var modifiedIssuesMap map[int]struct {Old Issue; New Issue}  
	var newIssues []Issue
	callback := func(dd git.DiffDelta, f float64) (git.DiffForEachHunkCallback, error) {
		switch dd.Status {
		case git.DeltaModified:
			oldIssue, newIssue := parseIssue(dd.OldFile), parseIssue(dd.NewFile)
			modifiedIssuesMap[oldIssue.Id] = struct {Old Issue; New Issue} { oldIssue, newIssue }
		case git.DeltaAdded:
			newIssue := parseIssue(dd.NewFile)
			mod, ok := modifiedIssuesMap[newIssue.Id]
			if ok {
				modifiedIssuesMap[newIssue.Id] = struct {Old Issue; New Issue} { mod.Old, newIssue }
			} else {
				modifiedIssuesMap[newIssue.Id] = struct {Old Issue; New Issue} { Issue{}, newIssue }
			}
		case git.DeltaDeleted:
			oldIssue := parseIssue(dd.OldFile)
			mod, ok := modifiedIssuesMap[oldIssue.Id]
			if ok {
				modifiedIssuesMap[oldIssue.Id] = struct {Old Issue; New Issue} { oldIssue, mod.New }
			} else {
				modifiedIssuesMap[oldIssue.Id] = struct {Old Issue; New Issue} { oldIssue, Issue{} }
			}
		}
		return nil, nil
	}
	if err = diff.ForEach(callback, git.DiffDetailFiles); err != nil {
		panic(err)
	}
	// temporary debug print
	fmt.Println("Following issues modified: ")
	for _, m := range modifiedIssuesMap {
		fmt.Println(issueToText(m.Old))
		fmt.Println(issueToText(m.New))
	}
	fmt.Println("Following issues : ")
	// return
	modifiedIssues := make([]struct {Old Issue; New Issue}, len(modifiedIssuesMap), len(modifiedIssuesMap))
	index := 0
	for _, m := range modifiedIssuesMap {
		modifiedIssues[index] = m
		index++
	}
	return CommitDiff { NewIssues : newIssues, ModifiedIssues: modifiedIssues}
}

func setIgnoreDenyCurrentBranch(rpath string) {
	// this is an ugly hack to add config record - git.Config interfaces didn't work for me... TBD...
	cfgpath := rpath + "/.git/config"
	file, err := os.Open(cfgpath)
	if err != nil {
		panic(err)
	}
	content := readTextFile(file)
	file.Close()
	file, err = os.OpenFile(cfgpath, os.O_WRONLY, 0666)
	defer file.Close()
	if err != nil {
		panic(err)
	}
	content = append(content, "\n[receive]")
	content = append(content, "        denyCurrentBranch = ignore\n")
	for _, str := range content {
		_, err := file.WriteString(str + "\n")
		if err != nil {
			panic(err)
		}
	}
}

// TODO: rewrite this method to avoid parsing all issues
func (r *issueRepository) GetIssue(id int) (Issue, bool) { 
	issues, _ := r.GetIssues() 
	for _, issue := range issues {
		if issue.Id == id {
			return issue, true
		}
	}
	return Issue{}, false
}

func (r *issueRepository) GetIssues() ([]Issue, []time.Time) {
	r.establishExclusiveRepoConnection()
	defer r.closeExclusiveRepoConnection()
	repo := r.repo

	idx, err := repo.Index()
	if err != nil {
		panic(err)
	}

	issuesCount := idx.EntryCount()
	issues := make([]Issue, issuesCount)
	timestamps := make([]time.Time, issuesCount)

	for i := 0; i < int(issuesCount); i++ {
		ientry, _ := idx.EntryByIndex(uint(i))
		path := ientry.Path
		issue := Issue{}
		parseIssuePropertiesFromRepoPath(path, &issue)
		file, err := os.OpenFile(r.path+"/"+path, os.O_RDONLY, 0666)
		if err != nil {
			panic(err)
		}
		defer file.Close()
		status := parseIssuePropertiesFromText(readTextFile(file), &issue)
		if status == false {
			panic("Issue parse failed")
		}
		issues[i] = issue
		timestamps[i] = ientry.Mtime
	}
	return issues, timestamps
}

func readTextFile(file *os.File) []string {
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

const delimiter string = "----------------------------------"

func parseIssuePropertiesFromRepoPath(path string, issue *Issue) {
	split := strings.Split(path, "/")
	issue.Opened = split[0] == "open"
	splitIndex := 1
	if split[splitIndex][0] != '@' && split[splitIndex][0] != '#' {
		issue.Milestone = split[splitIndex]
		splitIndex++
	}
	if split[splitIndex][0] == '@' {
		issue.Assignee = split[splitIndex][1:]
		splitIndex++
	}
	if split[splitIndex][0] == '#' {
		id, err := strconv.Atoi(split[splitIndex][1:])
		if err != nil {
			panic("Invalid issue id " + split[splitIndex][1:])
		}
		issue.Id = id
	} else {
		panic("Wrong issue path" + path)
	}
}

func parseIssuePropertiesFromText(text []string, issue *Issue) bool {
	i := 0
	textLength := len(text)
	// Parse Title
	for ; i < textLength; i++ {
		if strings.Contains(text[i], "Title:") {
			issue.Title = strings.TrimSpace(strings.Split(text[i], ":")[1])
			i++
			break
		}
	}
	if i == textLength {
		panic("panic")
		return false
	}
	// Parse Pull Request
	for ; i < textLength; i++ {
		if strings.Contains(text[i], "Patch:") {
			issue.PullRequest = strings.TrimSpace(strings.Split(text[i], ":")[1])
			i++
			break
		}
	}
	if i == textLength {
		panic("panic")
		return false
	}
	// Parse Labels
	for ; i < textLength; i++ {
		if strings.Contains(text[i], "Labels:") {
			split := strings.Split(text[i], ":")
			labels := strings.TrimSpace(split[1])
			if (len(labels) > 0) {
				split = strings.Split(labels, ",")
				for _, label := range split {
					issue.Labels = append(issue.Labels, strings.TrimSpace(label))
				}
			}	
			i++
			break
		}
	}
	if i == textLength {
		panic("panic")
		return false
	}
	// Parse description
	if text[i] == delimiter {
		i++
	} else {
		panic("panic")
		return false
	}
	for ; i < textLength; i++ {
		if text[i] == delimiter {
			break
		}
		issue.Description = issue.Description + text[i]
	}
	if i == textLength {
		return true
	}
	// Parse comments
	if text[i] == delimiter {
		i++
	} else {
		panic("panic")
		return false
	}
	comment := ""
	for ; i < textLength; i++ {
		if text[i] == delimiter {
			issue.Comments = append(issue.Comments, Comment{Text: comment})
			comment = ""
			continue
		}
		comment = comment + text[i]
	}
	return true
}

func issueToText(issue Issue) string {
	var buffer bytes.Buffer
	buffer.WriteString("Title: " + issue.Title + "\n\n")
	buffer.WriteString("Patch: " + issue.PullRequest + "\n\n")
	buffer.WriteString("Labels: ")
	for i, label := range issue.Labels {
		if i > 0 {
			buffer.WriteString(", ")
		}
		buffer.WriteString(label)
	}
	buffer.WriteString("\n" + delimiter + "\n")
	buffer.WriteString(issue.Description)
	buffer.WriteString("\n" + delimiter + "\n")
	for i, comment := range issue.Comments {
		if i > 0 {
			buffer.WriteString("\n" + delimiter + "\n")
		}
		buffer.WriteString(comment.Text)
	}
	buffer.WriteString("\n")
	return buffer.String()
}

func getIssueDir(issue Issue) string {
	var buffer bytes.Buffer

	if issue.Opened {
		buffer.WriteString("open/")
	} else {
		buffer.WriteString("close/")
	}

	if issue.Milestone != "" {
		buffer.WriteString(issue.Milestone + "/")
	}

	if issue.Assignee != "" {
		buffer.WriteString("@" + issue.Assignee + "/")
	}

	return buffer.String()
}

func getIssueFileName(issue Issue) string {
	return "#" + strconv.Itoa(issue.Id)
}

func mkIssueIdToPathMap(idx *git.Index) map[int]string {
	issuesCount := idx.EntryCount()
	idToPathMap := make(map[int]string)
	for i := 0; i < int(issuesCount); i++ {
		ientry, _ := idx.EntryByIndex(uint(i))
		split := strings.Split(ientry.Path, "#")
		id, _ := strconv.Atoi(split[len(split)-1])
		idToPathMap[id] = ientry.Path
	}
	return idToPathMap
}

func (r *issueRepository) StartCommitGroup() {
	r.establishExclusiveRepoConnection()
}

func (r *issueRepository) EndCommitGroup() {
	defer r.closeExclusiveRepoConnection()
}

func (r *issueRepository) establishExclusiveRepoConnection() {
	r.lock()
	repo, err := git.OpenRepository(r.path)
	if err != nil {
		panic(err)
	}
	copts := &git.CheckoutOpts{Strategy: git.CheckoutForce}
	repo.CheckoutHead(copts) // sync local dir
	r.repo = repo
}

func (r *issueRepository) closeExclusiveRepoConnection() {
	r.repo.Free()
	r.repo = nil
	r.unlock()
}

func (r *issueRepository) Commit(message string, issues []Issue, tm time.Time, updateAuthor *string) {
	if r.repo == nil { // Can be non-nil if we are inside commit group
		r.establishExclusiveRepoConnection()
		defer r.closeExclusiveRepoConnection()
	}

	repo := r.repo

	idx, err := repo.Index()
	if err != nil {
		panic(err)
	}

	idToPathMap := mkIssueIdToPathMap(idx)

	for _, issue := range issues {
		dir := r.path + "/" + getIssueDir(issue)
		repopath := getIssueDir(issue) + getIssueFileName(issue)
		filepath := r.path + "/" + repopath
		err := os.MkdirAll(dir, 0777)
		if err != nil {
			panic(err)
		}
		file, err := os.Create(filepath)
		if err != nil {
			panic(err)
		}
		file.WriteString(issueToText(issue))
		file.Close()
		err = idx.AddByPath(repopath)
		if err != nil {
			panic(err)
		}
		// if old path to issue was different, then we need to delete old version
		oldPath, ok := idToPathMap[issue.Id]
		if ok && (oldPath != repopath) {
			if err := os.Remove(r.path + "/" + oldPath); err != nil {
				panic(err)
			}
			if err := idx.RemoveByPath(oldPath); err != nil {
				panic(err)
			}
		}
	}
	treeId, err := idx.WriteTree()
	if err != nil {
		panic(err)
	}
	if err = idx.Write(); err != nil {
		panic(err)
	}
	tree, err := repo.LookupTree(treeId)
	if err != nil {
		panic(err)
	}
	head, _ := repo.Head()
	var headCommit *git.Commit
	if head != nil {
		headCommit, err = repo.LookupCommit(head.Target())
		if err != nil {
			panic(err)
		}
	}
	// check if author is the same
	author := ""
	if updateAuthor != nil {
		author = *updateAuthor
	} else {
		singleAuthor := true
		for _, issue := range issues {
			if author == "" {
				author = issue.Creator
			} else if author != issue.Creator {
				singleAuthor = false
				break
			}
		}
		if singleAuthor == false {
			author = "multiple authors"
		}
	}
	signature := &git.Signature{Name: author, Email: "none", When: tm}
	if headCommit != nil {
		_, err = repo.CreateCommit("refs/heads/master", signature, signature, message, tree, headCommit)
	} else {
		_, err = repo.CreateCommit("refs/heads/master", signature, signature, message, tree)
	}
	if err != nil {
		panic(err)
	}

	r.gcCounter++
	if r.gcCounter >= gcThreshold {
		r.doGarbageCollection()
		fmt.Println("Do garbage collection")
		r.gcCounter = 0
	}
}

func (r *issueRepository) Path() string {
	return r.path
}

func (r *issueRepository) doGarbageCollection() {
	// libgit2 API does not inlude explicit "gc" or "repack" command
	// It (API) is too low-level, so we would need to implement our own repack algorithm
	// Let's do this "hack" instead
	_, err := exec.Command("git", "--git-dir=" + r.path + "/.git", "gc").CombinedOutput()
	if err != nil {
		panic(err)
	}
}
