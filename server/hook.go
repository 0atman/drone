package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/square/go-jose"

	log "github.com/Sirupsen/logrus"
	"github.com/drone/drone/model"
	"github.com/drone/drone/remote"
	"github.com/drone/drone/shared/httputil"
	"github.com/drone/drone/shared/token"
	"github.com/drone/drone/store"
	"github.com/drone/drone/yaml"
	"github.com/drone/envsubst"
	"github.com/drone/mq/stomp"

	"github.com/cncd/pipeline/pipeline/backend"
	"github.com/cncd/pipeline/pipeline/frontend"
	yaml2 "github.com/cncd/pipeline/pipeline/frontend/yaml"
	"github.com/cncd/pipeline/pipeline/frontend/yaml/compiler"
	"github.com/cncd/pipeline/pipeline/frontend/yaml/linter"
	"github.com/cncd/pipeline/pipeline/frontend/yaml/matrix"
	"github.com/cncd/pipeline/pipeline/rpc"
	"github.com/cncd/pubsub"
	"github.com/cncd/queue"
)

var skipRe = regexp.MustCompile(`\[(?i:ci *skip|skip *ci)\]`)

func PostHook(c *gin.Context) {
	remote_ := remote.FromContext(c)

	tmprepo, build, err := remote_.Hook(c.Request)
	if err != nil {
		log.Errorf("failure to parse hook. %s", err)
		c.AbortWithError(400, err)
		return
	}
	if build == nil {
		c.Writer.WriteHeader(200)
		return
	}
	if tmprepo == nil {
		log.Errorf("failure to ascertain repo from hook.")
		c.Writer.WriteHeader(400)
		return
	}

	// skip the build if any case-insensitive combination of the words "skip" and "ci"
	// wrapped in square brackets appear in the commit message
	skipMatch := skipRe.FindString(build.Message)
	if len(skipMatch) > 0 {
		log.Infof("ignoring hook. %s found in %s", skipMatch, build.Commit)
		c.Writer.WriteHeader(204)
		return
	}

	repo, err := store.GetRepoOwnerName(c, tmprepo.Owner, tmprepo.Name)
	if err != nil {
		log.Errorf("failure to find repo %s/%s from hook. %s", tmprepo.Owner, tmprepo.Name, err)
		c.AbortWithError(404, err)
		return
	}

	// get the token and verify the hook is authorized
	parsed, err := token.ParseRequest(c.Request, func(t *token.Token) (string, error) {
		return repo.Hash, nil
	})
	if err != nil {
		log.Errorf("failure to parse token from hook for %s. %s", repo.FullName, err)
		c.AbortWithError(400, err)
		return
	}
	if parsed.Text != repo.FullName {
		log.Errorf("failure to verify token from hook. Expected %s, got %s", repo.FullName, parsed.Text)
		c.AbortWithStatus(403)
		return
	}

	if repo.UserID == 0 {
		log.Warnf("ignoring hook. repo %s has no owner.", repo.FullName)
		c.Writer.WriteHeader(204)
		return
	}
	var skipped = true
	if (build.Event == model.EventPush && repo.AllowPush) ||
		(build.Event == model.EventPull && repo.AllowPull) ||
		(build.Event == model.EventDeploy && repo.AllowDeploy) ||
		(build.Event == model.EventTag && repo.AllowTag) {
		skipped = false
	}

	if skipped {
		log.Infof("ignoring hook. repo %s is disabled for %s events.", repo.FullName, build.Event)
		c.Writer.WriteHeader(204)
		return
	}

	user, err := store.GetUser(c, repo.UserID)
	if err != nil {
		log.Errorf("failure to find repo owner %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	// if there is no email address associated with the pull request,
	// we lookup the email address based on the authors github login.
	//
	// my initial hesitation with this code is that it has the ability
	// to expose your email address. At the same time, your email address
	// is already exposed in the public .git log. So while some people will
	// a small number of people will probably be upset by this, I'm not sure
	// it is actually that big of a deal.
	if len(build.Email) == 0 {
		author, uerr := store.GetUserLogin(c, build.Author)
		if uerr == nil {
			build.Email = author.Email
		}
	}

	// if the remote has a refresh token, the current access token
	// may be stale. Therefore, we should refresh prior to dispatching
	// the job.
	if refresher, ok := remote_.(remote.Refresher); ok {
		ok, _ := refresher.Refresh(user)
		if ok {
			store.UpdateUser(c, user)
		}
	}

	// fetch the build file from the database
	config := ToConfig(c)
	raw, err := remote_.File(user, repo, build, config.Yaml)
	if err != nil {
		log.Errorf("failure to get build config for %s. %s", repo.FullName, err)
		c.AbortWithError(404, err)
		return
	}
	sec, err := remote_.File(user, repo, build, config.Shasum)
	if err != nil {
		log.Debugf("cannot find build secrets for %s. %s", repo.FullName, err)
		// NOTE we don't exit on failure. The sec file is optional
	}

	axes, err := yaml.ParseMatrix(raw)
	if err != nil {
		c.String(500, "Failed to parse yaml file or calculate matrix. %s", err)
		return
	}
	if len(axes) == 0 {
		axes = append(axes, yaml.Axis{})
	}

	netrc, err := remote_.Netrc(user, repo)
	if err != nil {
		c.String(500, "Failed to generate netrc file. %s", err)
		return
	}

	// verify the branches can be built vs skipped
	branches := yaml.ParseBranch(raw)
	if !branches.Match(build.Branch) && build.Event != model.EventTag && build.Event != model.EventDeploy {
		c.String(200, "Branch does not match restrictions defined in yaml")
		return
	}

	signature, err := jose.ParseSigned(string(sec))
	if err != nil {
		log.Debugf("cannot parse .drone.yml.sig file. %s", err)
	} else if len(sec) == 0 {
		log.Debugf("cannot parse .drone.yml.sig file. empty file")
	} else {
		build.Signed = true
		output, verr := signature.Verify([]byte(repo.Hash))
		if verr != nil {
			log.Debugf("cannot verify .drone.yml.sig file. %s", verr)
		} else if string(output) != string(raw) {
			log.Debugf("cannot verify .drone.yml.sig file. no match")
		} else {
			build.Verified = true
		}
	}

	// update some build fields
	build.Status = model.StatusPending
	build.RepoID = repo.ID

	// and use a transaction
	var jobs []*model.Job
	for num, axis := range axes {
		jobs = append(jobs, &model.Job{
			BuildID:     build.ID,
			Number:      num + 1,
			Status:      model.StatusPending,
			Environment: axis,
		})
	}
	err = store.CreateBuild(c, build, jobs...)
	if err != nil {
		log.Errorf("failure to save commit for %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	c.JSON(200, build)

	url := fmt.Sprintf("%s/%s/%d", httputil.GetURL(c.Request), repo.FullName, build.Number)
	err = remote_.Status(user, repo, build, url)
	if err != nil {
		log.Errorf("error setting commit status for %s/%d", repo.FullName, build.Number)
	}

	// get the previous build so that we can send
	// on status change notifications
	last, _ := store.GetBuildLastBefore(c, repo, build.Branch, build.ID)
	secs, err := store.GetMergedSecretList(c, repo)
	if err != nil {
		log.Debugf("Error getting secrets for %s#%d. %s", repo.FullName, build.Number, err)
	}

	client := stomp.MustFromContext(c)
	client.SendJSON("topic/events", model.Event{
		Type:  model.Enqueued,
		Repo:  *repo,
		Build: *build,
	},
		stomp.WithHeader("repo", repo.FullName),
		stomp.WithHeader("private", strconv.FormatBool(repo.IsPrivate)),
	)

	for _, job := range jobs {
		broker, _ := stomp.FromContext(c)
		broker.SendJSON("/queue/pending", &model.Work{
			Signed:    build.Signed,
			Verified:  build.Verified,
			User:      user,
			Repo:      repo,
			Build:     build,
			BuildLast: last,
			Job:       job,
			Netrc:     netrc,
			Yaml:      string(raw),
			Secrets:   secs,
			System:    &model.System{Link: httputil.GetURL(c.Request)},
		},
			stomp.WithHeader(
				"platform",
				yaml.ParsePlatformDefault(raw, "linux/amd64"),
			),
			stomp.WithHeaders(
				yaml.ParseLabel(raw),
			),
		)
	}

}

//
// CANARY IMPLEMENTATION
//
// This file is a complete disaster because I'm trying to wedge in some
// experimental code. Please pardon our appearance during renovations.
//

func GetQueueInfo(c *gin.Context) {
	c.IndentedJSON(200,
		config.queue.Info(c),
	)
}

func PostHook2(c *gin.Context) {
	remote_ := remote.FromContext(c)

	tmprepo, build, err := remote_.Hook(c.Request)
	if err != nil {
		log.Errorf("failure to parse hook. %s", err)
		c.AbortWithError(400, err)
		return
	}
	if build == nil {
		c.Writer.WriteHeader(200)
		return
	}
	if tmprepo == nil {
		log.Errorf("failure to ascertain repo from hook.")
		c.Writer.WriteHeader(400)
		return
	}

	// skip the build if any case-insensitive combination of the words "skip" and "ci"
	// wrapped in square brackets appear in the commit message
	skipMatch := skipRe.FindString(build.Message)
	if len(skipMatch) > 0 {
		log.Infof("ignoring hook. %s found in %s", skipMatch, build.Commit)
		c.Writer.WriteHeader(204)
		return
	}

	repo, err := store.GetRepoOwnerName(c, tmprepo.Owner, tmprepo.Name)
	if err != nil {
		log.Errorf("failure to find repo %s/%s from hook. %s", tmprepo.Owner, tmprepo.Name, err)
		c.AbortWithError(404, err)
		return
	}

	// get the token and verify the hook is authorized
	parsed, err := token.ParseRequest(c.Request, func(t *token.Token) (string, error) {
		return repo.Hash, nil
	})
	if err != nil {
		log.Errorf("failure to parse token from hook for %s. %s", repo.FullName, err)
		c.AbortWithError(400, err)
		return
	}
	if parsed.Text != repo.FullName {
		log.Errorf("failure to verify token from hook. Expected %s, got %s", repo.FullName, parsed.Text)
		c.AbortWithStatus(403)
		return
	}

	if repo.UserID == 0 {
		log.Warnf("ignoring hook. repo %s has no owner.", repo.FullName)
		c.Writer.WriteHeader(204)
		return
	}
	var skipped = true
	if (build.Event == model.EventPush && repo.AllowPush) ||
		(build.Event == model.EventPull && repo.AllowPull) ||
		(build.Event == model.EventDeploy && repo.AllowDeploy) ||
		(build.Event == model.EventTag && repo.AllowTag) {
		skipped = false
	}

	if skipped {
		log.Infof("ignoring hook. repo %s is disabled for %s events.", repo.FullName, build.Event)
		c.Writer.WriteHeader(204)
		return
	}

	user, err := store.GetUser(c, repo.UserID)
	if err != nil {
		log.Errorf("failure to find repo owner %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	// if there is no email address associated with the pull request,
	// we lookup the email address based on the authors github login.
	//
	// my initial hesitation with this code is that it has the ability
	// to expose your email address. At the same time, your email address
	// is already exposed in the public .git log. So while some people will
	// a small number of people will probably be upset by this, I'm not sure
	// it is actually that big of a deal.
	if len(build.Email) == 0 {
		author, uerr := store.GetUserLogin(c, build.Author)
		if uerr == nil {
			build.Email = author.Email
		}
	}

	// if the remote has a refresh token, the current access token
	// may be stale. Therefore, we should refresh prior to dispatching
	// the job.
	if refresher, ok := remote_.(remote.Refresher); ok {
		ok, _ := refresher.Refresh(user)
		if ok {
			store.UpdateUser(c, user)
		}
	}

	// fetch the build file from the database
	cfg := ToConfig(c)
	raw, err := remote_.File(user, repo, build, cfg.Yaml)
	if err != nil {
		log.Errorf("failure to get build config for %s. %s", repo.FullName, err)
		c.AbortWithError(404, err)
		return
	}
	sec, err := remote_.File(user, repo, build, cfg.Shasum)
	if err != nil {
		log.Debugf("cannot find build secrets for %s. %s", repo.FullName, err)
		// NOTE we don't exit on failure. The sec file is optional
	}

	axes, err := matrix.Parse(raw)
	if err != nil {
		c.String(500, "Failed to parse yaml file or calculate matrix. %s", err)
		return
	}
	if len(axes) == 0 {
		axes = append(axes, matrix.Axis{})
	}

	netrc, err := remote_.Netrc(user, repo)
	if err != nil {
		c.String(500, "Failed to generate netrc file. %s", err)
		return
	}

	// verify the branches can be built vs skipped
	branches, err := yaml2.ParseBytes(raw)
	if err != nil {
		c.String(500, "Failed to parse yaml file. %s", err)
		return
	}
	if !branches.Branches.Match(build.Branch) && build.Event != model.EventTag && build.Event != model.EventDeploy {
		c.String(200, "Branch does not match restrictions defined in yaml")
		return
	}

	signature, err := jose.ParseSigned(string(sec))
	if err != nil {
		log.Debugf("cannot parse .drone.yml.sig file. %s", err)
	} else if len(sec) == 0 {
		log.Debugf("cannot parse .drone.yml.sig file. empty file")
	} else {
		build.Signed = true
		output, verr := signature.Verify([]byte(repo.Hash))
		if verr != nil {
			log.Debugf("cannot verify .drone.yml.sig file. %s", verr)
		} else if string(output) != string(raw) {
			log.Debugf("cannot verify .drone.yml.sig file. no match")
		} else {
			build.Verified = true
		}
	}

	// update some build fields
	build.Status = model.StatusPending
	build.RepoID = repo.ID

	// and use a transaction
	var jobs []*model.Job
	for num, axis := range axes {
		jobs = append(jobs, &model.Job{
			BuildID:     build.ID,
			Number:      num + 1,
			Status:      model.StatusPending,
			Environment: axis,
		})
	}
	err = store.CreateBuild(c, build, jobs...)
	if err != nil {
		log.Errorf("failure to save commit for %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	c.JSON(200, build)

	uri := fmt.Sprintf("%s/%s/%d", httputil.GetURL(c.Request), repo.FullName, build.Number)
	err = remote_.Status(user, repo, build, uri)
	if err != nil {
		log.Errorf("error setting commit status for %s/%d", repo.FullName, build.Number)
	}

	// get the previous build so that we can send
	// on status change notifications
	last, _ := store.GetBuildLastBefore(c, repo, build.Branch, build.ID)
	secs, err := store.GetMergedSecretList(c, repo)
	if err != nil {
		log.Debugf("Error getting secrets for %s#%d. %s", repo.FullName, build.Number, err)
	}

	//
	// new code here
	//

	message := pubsub.Message{}
	message.Data, _ = json.Marshal(model.Event{
		Type:  model.Enqueued,
		Repo:  *repo,
		Build: *build,
	})
	message.Labels = map[string]string{
		"repo":    repo.FullName,
		"private": strconv.FormatBool(repo.IsPrivate),
	}
	// TODO remove global reference
	config.pubsub.Publish(c, "topic/events", message)

	//
	// workspace
	//

	var (
		link, _ = url.Parse(repo.Link)
		base    = "/drone"
		path    = "src/" + link.Host + "/" + repo.FullName
	)

	for _, job := range jobs {

		metadata := metadataFromStruct(repo, build, last, job, "linux/amd64")
		environ := metadata.Environ()

		secrets := map[string]string{}
		for _, sec := range secs {
			if !sec.MatchEvent(build.Event) {
				continue
			}
			if build.Verified || sec.SkipVerify {
				secrets[sec.Name] = sec.Value
			}
		}
		sub := func(name string) string {
			if v, ok := environ[name]; ok {
				return v
			}
			return secrets[name]
		}
		if s, err := envsubst.Eval(string(raw), sub); err != nil {
			raw = []byte(s)
		}
		parsed, err := yaml2.ParseBytes(raw)
		if err != nil {
			// TODO
		}

		lerr := linter.New(
			linter.WithTrusted(repo.IsTrusted),
		).Lint(parsed)
		if lerr != nil {
			// TODO
		}

		ir := compiler.New(
			compiler.WithEnviron(environ),
			compiler.WithEscalated("plugins/docker", "plugins/gcr", "plugins/ecr"),
			compiler.WithLocal(false),
			compiler.WithNetrc(netrc.Login, netrc.Password, netrc.Machine),
			compiler.WithPrefix(
				fmt.Sprintf(
					"%d_%d",
					job.ID,
					time.Now().Unix(),
				),
			),
			compiler.WithProxy(),
			compiler.WithVolumes(), // todo set global volumes
			compiler.WithWorkspace(base, path),
		).Compile(parsed)

		task := new(queue.Task)
		task.ID = fmt.Sprint(job.ID)
		task.Labels = map[string]string{}
		task.Labels["platform"] = "linux/amd64"
		// TODO set proper platform
		// TODO set proper labels
		task.Data, _ = json.Marshal(rpc.Pipeline{
			ID:      fmt.Sprint(job.ID),
			Config:  ir,
			Timeout: repo.Timeout,
		})

		config.logger.Open(context.Background(), task.ID)
		config.queue.Push(context.Background(), task)
	}
}

// use helper funciton to return ([]backend.Config, error)

type builder struct {
	secs  []*model.Secret
	repo  *model.Repo
	build *model.Build
	last  *model.Build
	jobs  []*model.Job
	link  string
}

func (b *builder) Build() ([]*backend.Config, error) {

	return nil, nil
}

// return the metadata from the cli context.
func metadataFromStruct(repo *model.Repo, build, last *model.Build, job *model.Job, link string) frontend.Metadata {
	return frontend.Metadata{
		Repo: frontend.Repo{
			Name:    repo.Name,
			Link:    repo.Link,
			Remote:  repo.Clone,
			Private: repo.IsPrivate,
		},
		Curr: frontend.Build{
			Number:   build.Number,
			Created:  build.Created,
			Started:  build.Started,
			Finished: build.Finished,
			Status:   build.Status,
			Event:    build.Event,
			Link:     build.Link,
			Target:   build.Deploy,
			Commit: frontend.Commit{
				Sha:     build.Commit,
				Ref:     build.Ref,
				Refspec: build.Refspec,
				Branch:  build.Branch,
				Message: build.Message,
				Author: frontend.Author{
					Name:   build.Author,
					Email:  build.Email,
					Avatar: build.Avatar,
				},
			},
		},
		Prev: frontend.Build{
			Number:   last.Number,
			Created:  last.Created,
			Started:  last.Started,
			Finished: last.Finished,
			Status:   last.Status,
			Event:    last.Event,
			Link:     last.Link,
			Target:   last.Deploy,
			Commit: frontend.Commit{
				Sha:     last.Commit,
				Ref:     last.Ref,
				Refspec: last.Refspec,
				Branch:  last.Branch,
				Message: last.Message,
				Author: frontend.Author{
					Name:   last.Author,
					Email:  last.Email,
					Avatar: last.Avatar,
				},
			},
		},
		Job: frontend.Job{
			Number: job.Number,
			Matrix: job.Environment,
		},
		Sys: frontend.System{
			Name: "drone",
			Link: link,
			Arch: "linux/amd64",
		},
	}
}
