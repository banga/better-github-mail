package bettermail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"google.golang.org/appengine"
	"google.golang.org/appengine/mail"
	"google.golang.org/appengine/datastore"
	"golang.org/x/net/context"
	"google.golang.org/appengine/log"
)

var templates map[string]*Template

func init() {
	templates = loadTemplates()

	http.HandleFunc("/hook", hookHandler)
	http.HandleFunc("/hook-test-harness", hookTestHarnessHandler)
	http.HandleFunc("/test-mail-send", testMailSendHandler)
	http.HandleFunc("/_ah/bounce", bounceHandler)
	http.HandleFunc("/test-subject", testSubjectHandler)
}

type EmailThread struct {
	CommitSHA string    `datastore:",noindex"`
	Subject string      `datastore:",noindex"`
}

func createThread(sha string, subject string, c context.Context) {
	thread := EmailThread {
	    CommitSHA: sha,
	    Subject: subject,
	}
	key := datastore.NewKey(c, "EmailThread", sha, 0, nil)
	_, err := datastore.Put(c, key, &thread)
	if err != nil {
        log.Errorf(c, "Error creating thread: %s", err)
	} else {
        log.Infof(c, "Created thread: %v", thread)
    }
}

func getSubjectForCommit(sha string, c context.Context) string {
    thread := new(EmailThread)
    key := datastore.NewKey(c, "EmailThread", sha, 0, nil)
	err := datastore.Get(c, key, thread)
	if err != nil {
	    log.Infof(c, "No thread found for SHA = %s", sha)
	    return ""
	}
	return thread.Subject
}

func hookHandler(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	eventType := r.Header.Get("X-Github-Event")
	message, err := handlePayload(eventType, r.Body, c)
	if err != nil {
		log.Errorf(c, "Error %s handling %s payload", err, eventType)
		http.Error(w, "Error handling payload", http.StatusInternalServerError)
		return
	}
	if message == nil {
		fmt.Fprint(w, "Unhandled event type: %s", eventType)
		log.Warningf(c, "Unhandled event type: %s", eventType)
		return
	}
	err = mail.Send(c, message)
	if err != nil {
		log.Errorf(c, "Could not send mail: %s", err)
		http.Error(w, "Could not send mail", http.StatusInternalServerError)
		return
	}
	log.Infof(c, "Sent mail to %s", message.To[0])
	fmt.Fprint(w, "OK")
}

func handlePayload(eventType string, payloadReader io.Reader, c context.Context) (*mail.Message, error) {
	decoder := json.NewDecoder(payloadReader)
	if eventType == "push" {
		var payload PushPayload
		err := decoder.Decode(&payload)
		if err != nil {
			return nil, err
		}
		return handlePushPayload(payload, c)
	} else if eventType == "commit_comment" {
		var payload CommitCommentPayload
		err := decoder.Decode(&payload)
		if err != nil {
			return nil, err
		}
		return handleCommitCommentPayload(payload, c)
	}
	return nil, nil
}

func handlePushPayload(payload PushPayload, c context.Context) (*mail.Message, error) {
	// TODO: allow location to be customized
	location, _ := time.LoadLocation("America/Los_Angeles")

	displayCommits := make([]DisplayCommit, 0)
	for i := range payload.Commits {
		displayCommits = append(displayCommits, newDisplayCommit(&payload.Commits[i], payload.Sender, payload.Repo, location, c))
	}
	branchName := (*payload.Ref)[11:]
	branchUrl := fmt.Sprintf("https://github.com/%s/tree/%s", *payload.Repo.FullName, branchName)
	pushedDate := payload.Repo.PushedAt.In(location)
	// Last link is a link so that the GitHub Gmail extension
	// (https://github.com/muan/github-gmail) will open the diff view.
	extensionUrl := displayCommits[0].URL
	if len(displayCommits) > 1 {
		extensionUrl = *payload.Compare
	}
	var data = map[string]interface{}{
		"Payload":                  payload,
		"Commits":                  displayCommits,
		"BranchName":               branchName,
		"BranchURL":                branchUrl,
		"PushedDisplayDate":        safeFormattedDate(pushedDate.Format(DisplayDateFormat)),
		"PushedDisplayDateTooltip": pushedDate.Format(DisplayDateFullFormat),
		"ExtensionURL":             extensionUrl,
	}
	var mailHtml bytes.Buffer
	if err := templates["push"].Execute(&mailHtml, data); err != nil {
		return nil, err
	}

	senderUserName := *payload.Pusher.Name
	senderName := senderUserName
	// We don't have the display name in the pusher, but usually it's one of the
	// commiters, so get it from there (without having to do any extra API
	// requests)
	for _, commit := range payload.Commits {
		if *commit.Author.Username == senderUserName {
			senderName = *commit.Author.Name
			break
		}
		if *commit.Committer.Username == senderUserName {
			senderName = *commit.Committer.Name
			break
		}
	}

	sender := fmt.Sprintf("%s <%s@%s.appspotmail.com>", senderName, senderUserName, appengine.AppID(c))
	subjectCommit := displayCommits[0]
	subject := fmt.Sprintf("[%s] %s: %s", *payload.Repo.FullName, subjectCommit.ShortSHA, subjectCommit.Title)

	for _, commit := range displayCommits {
	    createThread(commit.SHA, subject, c)
	}

	message := &mail.Message{
		Sender:   sender,
		To:       []string{getRecipient()},
		Subject:  subject,
		HTMLBody: mailHtml.String(),
	}
	return message, nil
}

func handleCommitCommentPayload(payload CommitCommentPayload, c context.Context) (*mail.Message, error) {
	// TODO: allow location to be customized
	location, _ := time.LoadLocation("America/Los_Angeles")
	updatedDate := payload.Comment.UpdatedAt.In(location)

	commitSHA := *payload.Comment.CommitID
	commitShortSHA := commitSHA[:7]
	commitURL := *payload.Repo.URL + "/commit/" + commitSHA

	body := *payload.Comment.Body
	if len(body) > 0 {
	    body = renderMessageMarkdown(body, payload.Repo, c)
	}

	var data = map[string]interface{}{
		"Payload":                  payload,
		"Comment":                  payload.Comment,
		"Sender":                   payload.Sender,
		"Repo":                     payload.Repo,
		"ShortSHA":                 commitShortSHA,
		"Body":                     body,
		"CommitURL":                commitURL,
		"UpdatedDisplayDate":       safeFormattedDate(updatedDate.Format(DisplayDateFormat)),
	}

	var mailHtml bytes.Buffer
	if err := templates["commit-comment"].Execute(&mailHtml, data); err != nil {
		return nil, err
	}

	senderUserName := *payload.Sender.Login
	senderName := senderUserName

	sender := fmt.Sprintf("%s <%s@%s.appspotmail.com>", senderName, senderUserName, appengine.AppID(c))
	subject := getSubjectForCommit(commitSHA, c)
	if subject == "" {
		subject = fmt.Sprintf("[%s] %s", *payload.Repo.FullName, commitShortSHA)
	}

	message := &mail.Message{
		Sender:   sender,
		To:       []string{getRecipient()},
		Subject:  subject,
		HTMLBody: mailHtml.String(),
	}
	return message, nil
}

func getRecipient() string {
	if appengine.IsDevAppServer() {
		return "mihai@quip.com"
	}
	return "eng+commits@quip.com"
}

func hookTestHarnessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		templates["hook-test-harness"].Execute(w, nil)
		return
	}
	if r.Method == "POST" {
		eventType := r.FormValue("event_type")
		payload := r.FormValue("payload")
		c := appengine.NewContext(r)

		message, err := handlePayload(eventType, strings.NewReader(payload), c)
		var data = map[string]interface{}{
			"EventType":  eventType,
			"Payload":    payload,
			"Message":    message,
			"MessageErr": err,
		}
		templates["hook-test-harness"].Execute(w, data)
		return
	}
	http.Error(w, "", http.StatusMethodNotAllowed)
}

func bounceHandler(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	if b, err := ioutil.ReadAll(r.Body); err == nil {
		log.Warningf(c, "Bounce: %s", string(b))
	} else {
		log.Warningf(c, "Bounce: <unreadable body>")
	}
}

func testSubjectHandler(w http.ResponseWriter, r *http.Request) {
	if !appengine.IsDevAppServer() {
	    http.Error(w, "", http.StatusMethodNotAllowed)
	    return
	}
	values := r.URL.Query()
	sha, ok := values["sha"]
	if !ok || len(sha) < 1 {
	    http.Error(w, "Need to specify sha param", http.StatusInternalServerError)
	    return
	}
	c := appengine.NewContext(r)
	subject := getSubjectForCommit(sha[0], c)
	fmt.Fprintf(w, "%s\n", subject)
}

func testMailSendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		templates["test-mail-send"].Execute(w, nil)
		return
	}
	if r.Method == "POST" {
		c := appengine.NewContext(r)
		message := &mail.Message{
			Sender:   r.FormValue("sender"),
			To:       []string{"mihai@quip.com"},
			Subject:  r.FormValue("subject"),
			HTMLBody: r.FormValue("html_body"),
		}
		err := mail.Send(c, message)
		var data = map[string]interface{}{
			"Message": message,
			"SendErr": err,
		}
		templates["test-mail-send"].Execute(w, data)
		return
	}
	http.Error(w, "", http.StatusMethodNotAllowed)
}
