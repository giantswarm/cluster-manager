package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/provenance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/identity"
)

// The Flux objects commit mode follows from a cluster to the repository that
// owns it.
var (
	KustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	GitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
)

const (
	// labelKustomizeNamespace is the namespace of the Kustomization named by
	// labelKustomizeName.
	labelKustomizeNamespace = "kustomize.toolkit.fluxcd.io/namespace"

	// CommitDirectory is the directory cluster-manager writes a cluster's
	// releases to, under the directory its Kustomization builds from. It
	// carries its own kustomization.yaml listing every file in it, so Flux
	// builds it whether the parent directory has a kustomization.yaml (the
	// entry is added there) or not (Flux generates one that includes every
	// directory with a kustomization.yaml).
	CommitDirectory = "cluster-manager"
)

// File actions of a commit.
const (
	fileAdd       = string(layout.Add)
	fileUpdate    = string(layout.Update)
	fileRemove    = string(layout.Remove)
	fileUnchanged = string(layout.Unchanged)
)

// GitHubRemote is what commit mode needs of GitHub: the pull request seams
// and the reads of the base (gitops-commit's Remote and Reader), acting as
// the person.
type GitHubRemote interface {
	commit.Remote
	commit.Reader
}

// RemoteFor builds the GitHub remote from the person's App user token.
type RemoteFor func(token string) (GitHubRemote, error)

// WithGitHub offers commit mode: the pull request is opened as the person
// with the GitHub token the App-pinned registration carries.
func WithGitHub(remote RemoteFor) Option {
	return func(s *Service) { s.remote = remote }
}

// CommitAvailable reports whether this server offers commit mode.
func (s *Service) CommitAvailable() bool { return s.remote != nil }

// CommitResult is the pull request a write in commit mode opened — or, dry
// run, would open — in the repository that owns the cluster.
type CommitResult struct {
	// Repository is owner/name, Base the branch the Kustomization follows.
	Repository string `json:"repository"`
	Base       string `json:"base"`
	// Directory is where the files go: CommitDirectory under the
	// Kustomization's path.
	Directory string `json:"directory"`
	// Kustomization is the Flux Kustomization (namespace/name) that lands
	// the files after the merge, and Prune its spec.prune: whether files
	// removed from git are removed from the installation too.
	Kustomization string `json:"kustomization"`
	Prune         bool   `json:"prune"`
	// Branch is the pull request's head branch.
	Branch string       `json:"branch"`
	Files  []CommitFile `json:"files"`
	// PullRequest is the URL of the pull request opened as the caller, empty
	// on a dry run and when nothing changes.
	PullRequest string `json:"pullRequest,omitempty"`
	Number      int    `json:"number,omitempty"`
	// Author is the GitHub login the pull request is opened as.
	Author string `json:"author,omitempty"`
	// LiveSteps are what the merge alone does not do on the installation.
	LiveSteps []string `json:"liveSteps,omitempty"`
}

// CommitFile is one file of the commit. Content is shown on a dry run,
// never for a secret file.
type CommitFile struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content,omitempty"`
}

// commitLocation is where a cluster's releases go in git and how Flux lands
// them.
type commitLocation struct {
	provenance.Location
	kustomization string
	prune         bool
}

// directory is cluster-manager's directory in the repository.
func (l commitLocation) directory() layout.Directory {
	return layout.Directory{Location: l.Location, Name: CommitDirectory}
}

// dir is the repository-relative directory cluster-manager writes to.
func (l commitLocation) dir() string { return l.directory().Path() }

// target is the answer's view of the location.
func (l commitLocation) target() *CommitTarget {
	return &CommitTarget{Repository: l.Repository.String(), Branch: l.Branch, Path: l.dir(), Kustomization: l.kustomization, Prune: l.prune}
}

// errNotFromGit is a cluster no Flux Kustomization owns.
var errNotFromGit = errors.New("not reconciled from git")

// commitLocationOf follows the cluster to the repository that owns it: the
// Flux Kustomization labels on the cluster's HelmRelease or App (named like
// the cluster), else on the Cluster itself, then the Kustomization's
// GitRepository and path. Read as the caller.
func commitLocationOf(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (commitLocation, error) {
	ns, name := c.GetNamespace(), c.GetName()
	owner := c
	for _, gvr := range []schema.GroupVersionResource{HelmReleaseGVR, AppGVR} {
		if obj, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			owner = obj
			break
		}
	}
	ksName := owner.GetLabels()[labelKustomizeName]
	if ksName == "" {
		return commitLocation{}, fmt.Errorf("%w: no Flux Kustomization owns cluster %s/%s (no %s label on its %s)", errNotFromGit, ns, name, labelKustomizeName, owner.GetKind())
	}
	ksNamespace := owner.GetLabels()[labelKustomizeNamespace]
	ks, err := dyn.Resource(KustomizationGVR).Namespace(ksNamespace).Get(ctx, ksName, metav1.GetOptions{})
	if err != nil {
		return commitLocation{}, fmt.Errorf("get Kustomization %s/%s owning cluster %s: %w", ksNamespace, ksName, name, err)
	}
	src := provenance.SourceRef{
		Kind:      nestedString(ks, "spec", "sourceRef", "kind"),
		Name:      nestedString(ks, "spec", "sourceRef", "name"),
		Namespace: nestedString(ks, "spec", "sourceRef", "namespace"),
	}
	flux := provenance.Flux{Kustomizations: []provenance.Kustomization{{Name: ksName, Namespace: ksNamespace, SourceRef: src, Path: nestedString(ks, "spec", "path")}}}
	if src.Kind == provenance.KindGitRepository {
		srcNamespace := src.Namespace
		if srcNamespace == "" {
			srcNamespace = ksNamespace
		}
		repo, err := dyn.Resource(GitRepositoryGVR).Namespace(srcNamespace).Get(ctx, src.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return commitLocation{}, fmt.Errorf("get GitRepository %s/%s: %w", srcNamespace, src.Name, err)
		}
		if err == nil {
			flux.GitRepositories = []provenance.GitRepository{{Name: src.Name, Namespace: srcNamespace, URL: nestedString(repo, "spec", "url"), Branch: nestedString(repo, "spec", "ref", "branch")}}
		}
	}
	loc, err := flux.Resolve(ksNamespace, ksName)
	if err != nil {
		return commitLocation{}, fmt.Errorf("cluster %s: %w", name, err)
	}
	prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune")
	return commitLocation{Location: loc, kustomization: ksNamespace + "/" + ksName, prune: prune}, nil
}

// commitLocationFor is commitLocationOf for a write in commit mode: a
// cluster that is not reconciled from git is a refusal with the way out.
func commitLocationFor(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (commitLocation, error) {
	loc, err := commitLocationOf(ctx, dyn, c)
	if errors.Is(err, errNotFromGit) {
		return loc, &ErrRefused{Reason: err.Error() + ": commit mode writes into the repository that owns the cluster, and this one has none — use mode apply"}
	}
	return loc, err
}

// gitHubFor is the caller's GitHub remote, or the refusal naming the
// consent that gives one.
func (s *Service) gitHubFor(ctx context.Context) (GitHubRemote, *identity.GitHub, error) {
	gh, ok := identity.GitHubFromContext(ctx)
	if !ok {
		return nil, nil, &ErrRefused{Reason: "commit mode opens the pull request with your GitHub authorization, and this call carries none: connect cluster-manager in muster (core_auth_login server=cluster-manager), then call again"}
	}
	remote, err := s.remote(gh.Token)
	if err != nil {
		return nil, nil, commitError(err)
	}
	return remote, gh, nil
}

// commitError answers a token GitHub refused with the consent to renew.
func commitError(err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return &ErrRefused{Reason: fmt.Sprintf("GitHub refused your token on %s (status %d): reconnect cluster-manager in muster (core_auth_login server=cluster-manager) — the App must be installed on the repository and your account must be allowed to push to it", auth.Op, auth.Status)}
	}
	return err
}

// releaseFiles renders the composed objects as the files of the cluster's
// directory: one file per release (its source and the HelmRelease, named
// after them), a Secret in a file of its own named as a secret file. The
// ownerReferences apply mode sets to the live Cluster are left out: they name
// that object's UID, which a file in git must not carry — the cluster
// recreated from the same repository would have its pools collected — and
// the Kustomization that owns the cluster's files removes them with it.
func releaseFiles(dir layout.Directory, objs []*unstructured.Unstructured) (map[string][]byte, error) {
	docs := map[string][][]byte{}
	var order []string
	for _, obj := range objs {
		obj = obj.DeepCopy()
		obj.SetOwnerReferences(nil)
		p := dir.ObjectFile(obj.GetKind(), obj.GetName())
		raw, err := sigsyaml.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("render %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		if _, seen := docs[p]; !seen {
			order = append(order, p)
		}
		docs[p] = append(docs[p], raw)
	}
	files := make(map[string][]byte, len(order))
	for _, p := range order {
		files[p] = bytes.Join(docs[p], []byte("---\n"))
	}
	return files, nil
}

// commitPlan is the change set of one write in cluster-manager's directory
// (gitops-commit's layout) with the remote that lands it.
type commitPlan struct {
	*layout.Plan
	loc    commitLocation
	remote GitHubRemote
}

// readBase reads a file at the base branch; nil without error when absent.
func readBase(ctx context.Context, remote GitHubRemote, loc commitLocation, p string) ([]byte, error) {
	raw, err := remote.ReadFile(ctx, loc.Repository, loc.Branch, p)
	if errors.Is(err, commit.ErrFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, commitError(err)
	}
	return raw, nil
}

// planCommit decides every file of the change against the base
// (layout.Build): written files, removed files that exist, a secret file
// encrypted for the repository's .sops.yaml or left as it is when it exists,
// and the kustomization entries of the directory and its parent.
func planCommit(ctx context.Context, remote GitHubRemote, loc commitLocation, write map[string][]byte, remove []string) (*commitPlan, error) {
	plan, err := layout.Build(ctx, remote, loc.directory(), write, remove)
	var secret *layout.SecretError
	if errors.As(err, &secret) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("%s holds the pool's registry credentials, and %s: cluster-manager commits a secret only encrypted for the repository's age recipients — use mode apply, or give the path an age creation rule", strings.Join(secret.Paths, ", "), secret.Reason)}
	}
	if err != nil {
		return nil, commitError(err)
	}
	return &commitPlan{Plan: plan, loc: loc, remote: remote}, nil
}

// actions are the plan's files as the answer reports them.
func (p *commitPlan) actions() []CommitFile {
	out := make([]CommitFile, len(p.Files))
	for i, f := range p.Files {
		out[i] = CommitFile{Path: f.Path, Action: string(f.Action), Content: string(f.Content)}
	}
	return out
}

// open lands the plan as one pull request as the caller, or reports it on a
// dry run. Nothing to change opens none.
func (p *commitPlan) open(ctx context.Context, gh *identity.GitHub, branch, title, body string, dryRun bool) (*CommitResult, error) {
	out := &CommitResult{
		Repository: p.loc.Repository.String(), Base: p.loc.Branch, Directory: p.loc.dir(),
		Kustomization: p.loc.kustomization, Prune: p.loc.prune, Branch: branch, Files: p.actions(), Author: gh.Login,
	}
	if !dryRun {
		for i := range out.Files {
			out.Files[i].Content = ""
		}
	}
	if dryRun || !p.Changed() {
		return out, nil
	}
	prs, err := commit.Open(ctx, p.remote, commit.Request{Branch: branch, Title: title, Body: body}, []commit.Change{p.Change()})
	if err != nil {
		return nil, commitError(err)
	}
	out.PullRequest, out.Number = prs[0].URL, prs[0].Number
	return out, nil
}
