package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
)

func (g *Github) GetBranches() (sourceBranch, workingBranch, targetBranch string) {
	sourceBranch = g.Spec.Branch
	workingBranch = g.Spec.Branch
	targetBranch = g.Spec.Branch

	if len(g.pipelineID) > 0 && g.workingBranch {
		workingBranch = g.nativeGitHandler.SanitizeBranchName(fmt.Sprintf("updatecli_%s_%s", targetBranch, g.pipelineID))
	}

	return sourceBranch, workingBranch, targetBranch
}

// GetURL returns a "GitHub " git URL
func (g *Github) GetURL() string {
	URL, err := url.JoinPath(g.Spec.URL, g.Spec.Owner, g.Spec.Repository+".git")
	if err != nil {
		logrus.Errorln(err)
		return ""
	}

	return URL
}

// GetDirectory returns the local git repository path.
func (g *Github) GetDirectory() (directory string) {
	return g.Spec.Directory
}

// Clean deletes github working directory.
func (g *Github) Clean() error {
	err := os.RemoveAll(g.Spec.Directory)
	if err != nil {
		return err
	}
	return nil
}

// Clone run `git clone`.
func (g *Github) Clone() (string, error) {
	g.setDirectory()

	err := g.nativeGitHandler.Clone(
		g.Spec.Username,
		g.Spec.Token,
		g.GetURL(),
		g.GetDirectory(),
		g.Spec.Submodules,
	)
	if err != nil {
		logrus.Errorf("failed cloning GitHub repository %q", g.GetURL())
		return "", err
	}

	return g.Spec.Directory, nil
}

// Commit run `git commit`.
func (g *Github) Commit(message string) error {

	// Generate the conventional commit message
	commitMessage, err := g.Spec.CommitMessage.Generate(message)
	if err != nil {
		return err
	}

	if g.commitUsingApi {
		err = g.CreateCommit(commitMessage)
		if err != nil {
			return err
		}
	} else {
		err = g.nativeGitHandler.Commit(
			g.Spec.User,
			g.Spec.Email,
			commitMessage,
			g.GetDirectory(),
			g.Spec.GPG.SigningKey,
			g.Spec.GPG.Passphrase,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

type githubCommit struct {
	CreateCommitOnBranch struct {
		Commit struct {
			URL string
		}
	} `graphql:"createCommitOnBranch(input:$input)"`
}

func (g *Github) CreateCommit(commitMessage string) error {
	var m githubCommit

	workingDir := g.GetDirectory()
	files, err := g.nativeGitHandler.GetChangedFiles(workingDir)
	if err != nil {
		return err
	}
	// process added / modified files:
	additions := make([]githubv4.FileAddition, 0, len(files))
	for _, f := range files {
		fullPath := fmt.Sprintf("%s/%s", workingDir, f)
		enc, err := base64EncodeFile(fullPath)
		if err != nil {
			return err
		}
		additions = append(additions, githubv4.FileAddition{
			Path:     githubv4.String(f),
			Contents: githubv4.Base64String(enc),
		})
	}
	// deletions := make([]githubv4.FileDeletion, 0, 0)

	// expectedHeadOid := g.nativeGitHandler.
	repositoryName := fmt.Sprintf("%s/%s", g.Spec.Owner, g.Spec.Repository)
	headOid, err := g.nativeGitHandler.GetLatestCommitHash(workingDir)
	if err != nil {
		return err
	}

	input := githubv4.CreateCommitOnBranchInput{
		Branch: githubv4.CommittableBranch{
			RepositoryNameWithOwner: githubv4.NewString(githubv4.String(repositoryName)),
			BranchName:              githubv4.NewString(githubv4.String(g.Spec.Branch)),
		},
		ExpectedHeadOid: githubv4.GitObjectID(headOid),
		Message: githubv4.CommitMessage{
			Headline: githubv4.String(commitMessage),
		},
		FileChanges: &githubv4.FileChanges{
			Additions: &additions,
			// Deletions: &deletions,
		},
	}

	if err := g.client.Mutate(context.Background(), &m, input, nil); err != nil {
		return err
	}
	return nil
}

// Checkout create and then uses a temporary git branch.
func (g *Github) Checkout() error {
	sourceBranch, workingBranch, _ := g.GetBranches()

	return g.nativeGitHandler.Checkout(
		g.Spec.Username,
		g.Spec.Token,
		sourceBranch,
		workingBranch,
		g.Spec.Directory,
		g.force,
	)
}

// Add run `git add`.
func (g *Github) Add(files []string) error {
	err := g.nativeGitHandler.Add(files, g.Spec.Directory)
	if err != nil {
		return err
	}
	return nil
}

// IsRemoteBranchUpToDate checks if the branch reference name is published on
// on the default remote
func (g *Github) IsRemoteBranchUpToDate() (bool, error) {
	sourceBranch, workingBranch, _ := g.GetBranches()

	return g.nativeGitHandler.IsLocalBranchPublished(
		sourceBranch,
		workingBranch,
		g.Spec.Username,
		g.Spec.Token,
		g.GetDirectory())
}

// Push run `git push` on the GitHub remote branch if not already created.
func (g *Github) Push() (bool, error) {

	return g.nativeGitHandler.Push(
		g.Spec.Username,
		g.Spec.Token,
		g.GetDirectory(),
		g.force,
	)
}

// PushTag push tags
func (g *Github) PushTag(tag string) error {

	err := g.nativeGitHandler.PushTag(
		tag,
		g.Spec.Username,
		g.Spec.Token,
		g.GetDirectory(),
		g.force,
	)
	if err != nil {
		return err
	}

	return nil
}

// PushBranch push tags
func (g *Github) PushBranch(branch string) error {

	err := g.nativeGitHandler.PushBranch(
		branch,
		g.Spec.Username,
		g.Spec.Token,
		g.GetDirectory(),
		g.force)
	if err != nil {
		return err
	}

	return nil
}

func (g *Github) GetChangedFiles(workingDir string) ([]string, error) {
	return g.nativeGitHandler.GetChangedFiles(workingDir)
}

func base64EncodeFile(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()

	buf := bytes.Buffer{}
	encoder := base64.NewEncoder(base64.StdEncoding, &buf)

	if _, err := io.Copy(encoder, in); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}
