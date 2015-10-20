package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/rackerlabs/carina/version"
	"github.com/rackerlabs/libcarina"
)

// Application is, our, well, application
type Application struct {
	*Context
	*kingpin.Application
}

// Command is a command needing a ClusterClient
type Command struct {
	*Context
	*kingpin.CmdClause
}

// Context context for the  App
type Context struct {
	ClusterClient *libcarina.ClusterClient
	TabWriter     *tabwriter.Writer
	Username      string
	APIKey        string
	Endpoint      string
}

// ClusterCommand is a Command with a ClusterName set
type ClusterCommand struct {
	*Command
	ClusterName string
}

// CredentialsCommand keeps context about the download command
type CredentialsCommand struct {
	*ClusterCommand
	Path string
}

// WaitClusterCommand is simply a ClusterCommand that waits for cluster creation
type WaitClusterCommand struct {
	*ClusterCommand
	// Whether to wait until the cluster is created (or errored)
	Wait bool
}

// CreateCommand keeps context about the create command
type CreateCommand struct {
	*WaitClusterCommand

	// Options passed along to Carina's API
	Nodes     int
	AutoScale bool

	// TODO: See if setting flavor or image makes sense, even if the API takes it
	// Flavor    string
	// Image     string
}

// GrowCommand keeps context about the number of nodes to scale by
type GrowCommand struct {
	*ClusterCommand
	Nodes int
}

// New creates a new Application
func New() *Application {

	app := kingpin.New("carina", "command line interface to launch and work with Docker Swarm clusters")
	app.Version(VersionString())

	cap := new(Application)
	ctx := new(Context)

	cap.Application = app

	cap.Context = ctx

	cap.Flag("username", "Rackspace username - can also set env var RACKSPACE_USERNAME").OverrideDefaultFromEnvar("RACKSPACE_USERNAME").StringVar(&ctx.Username)
	cap.Flag("api-key", "Rackspace API Key - can also set env var RACKSPACE_APIKEY").OverrideDefaultFromEnvar("RACKSPACE_APIKEY").PlaceHolder("RACKSPACE_APIKEY").StringVar(&ctx.APIKey)
	cap.Flag("endpoint", "Carina API endpoint").Default(libcarina.BetaEndpoint).StringVar(&ctx.Endpoint)

	writer := new(tabwriter.Writer)
	writer.Init(os.Stdout, 0, 8, 1, '\t', 0)

	// Make sure the tabwriter gets flushed at the end
	app.Terminate(func(code int) {
		// Squish any errors from flush, since we're terminating the app anyway
		_ = ctx.TabWriter.Flush()
		os.Exit(code)
	})

	cap.Flag("bash-completion", "Generate bash completion").Action(cap.generateBashCompletion).Hidden()

	ctx.TabWriter = writer

	createCommand := new(CreateCommand)
	createCommand.WaitClusterCommand = cap.NewWaitClusterCommand(ctx, "create", "Create a swarm cluster")
	createCommand.Flag("nodes", "number of nodes for the initial cluster").Default("1").IntVar(&createCommand.Nodes)
	createCommand.Flag("autoscale", "whether autoscale is on or off").BoolVar(&createCommand.AutoScale)
	createCommand.Action(createCommand.Create)

	getCommand := cap.NewClusterCommand(ctx, "get", "Get information about a swarm cluster")
	getCommand.Action(getCommand.Get)

	listCommand := cap.NewCommand(ctx, "list", "List swarm clusters")
	listCommand.Action(listCommand.List)

	credentialsCommand := new(CredentialsCommand)
	credentialsCommand.ClusterCommand = cap.NewClusterCommand(ctx, "credentials", "Download credentials for a swarm cluster")
	credentialsCommand.Flag("path", "path to write credentials out to").PlaceHolder("<cluster-name>").StringVar(&credentialsCommand.Path)
	credentialsCommand.Action(credentialsCommand.Download)

	growCommand := new(GrowCommand)
	growCommand.ClusterCommand = cap.NewClusterCommand(ctx, "grow", "Grow a cluster by the requested number of nodes")
	growCommand.Flag("nodes", "number of nodes to increase the cluster by").Required().IntVar(&growCommand.Nodes)
	growCommand.Action(growCommand.Grow)

	rebuildCommand := cap.NewWaitClusterCommand(ctx, "rebuild", "Rebuild a swarm cluster")
	rebuildCommand.Action(rebuildCommand.Rebuild)

	deleteCommand := cap.NewClusterCommand(ctx, "delete", "Delete a swarm cluster")
	deleteCommand.Action(deleteCommand.Delete)

	return cap
}

// VersionString returns the current version and commit of this binary (if set)
func VersionString() string {
	s := ""
	s += fmt.Sprintf("Version: %s\n", version.Version)
	s += fmt.Sprintf("Commit:  %s", version.Commit)
	return s
}

// NewCommand creates a command wrapped with carina.Context
func (app *Application) NewCommand(ctx *Context, name, help string) *Command {
	carina := new(Command)
	carina.Context = ctx
	carina.CmdClause = app.Command(name, help)
	carina.PreAction(carina.Auth)
	return carina
}

// NewClusterCommand is a command that uses a cluster name
func (app *Application) NewClusterCommand(ctx *Context, name, help string) *ClusterCommand {
	cc := new(ClusterCommand)
	cc.Command = app.NewCommand(ctx, name, help)
	cc.Arg("cluster-name", "name of the cluster").Required().StringVar(&cc.ClusterName)
	return cc
}

// NewWaitClusterCommand is a command that uses a cluster name and allows the
// user to wait for a cluster state
func (app *Application) NewWaitClusterCommand(ctx *Context, name, help string) *WaitClusterCommand {
	wcc := new(WaitClusterCommand)
	wcc.ClusterCommand = app.NewClusterCommand(ctx, name, help)
	wcc.Flag("wait", "wait for swarm cluster to come online (or error)").BoolVar(&wcc.Wait)
	return wcc
}

// Auth does the authentication
func (carina *Command) Auth(pc *kingpin.ParseContext) (err error) {
	carina.ClusterClient, err = libcarina.NewClusterClient(carina.Endpoint, carina.Username, carina.APIKey)
	return err
}

// List the current swarm clusters
func (carina *Command) List(pc *kingpin.ParseContext) (err error) {
	clusterList, err := carina.ClusterClient.List()
	if err != nil {
		return err
	}

	err = writeClusterHeader(carina.TabWriter)
	if err != nil {
		return err
	}

	for _, cluster := range clusterList {
		err = writeCluster(carina.TabWriter, &cluster)
		if err != nil {
			return err
		}
	}
	err = carina.TabWriter.Flush()
	return err
}

type clusterOp func(clusterName string) (*libcarina.Cluster, error)

// Does an func against a cluster then returns the new cluster representation
func (carina *ClusterCommand) clusterApply(op clusterOp) (err error) {
	cluster, err := op(carina.ClusterName)
	if err != nil {
		return err
	}

	err = writeCluster(carina.TabWriter, cluster)
	if err != nil {
		return err
	}
	return carina.TabWriter.Flush()
}

// Get an individual cluster
func (carina *ClusterCommand) Get(pc *kingpin.ParseContext) (err error) {
	return carina.clusterApply(carina.ClusterClient.Get)
}

// Delete a cluster
func (carina *ClusterCommand) Delete(pc *kingpin.ParseContext) (err error) {
	return carina.clusterApply(carina.ClusterClient.Delete)
}

// Grow increases the size of the given cluster
func (carina *GrowCommand) Grow(pc *kingpin.ParseContext) (err error) {
	return carina.clusterApply(func(clusterName string) (*libcarina.Cluster, error) {
		return carina.ClusterClient.Grow(clusterName, carina.Nodes)
	})
}

// Rebuild nukes your cluster and builds it over again
func (carina *WaitClusterCommand) Rebuild(pc *kingpin.ParseContext) (err error) {
	return carina.clusterApplyWait(carina.ClusterClient.Rebuild)
}

const startupFudgeFactor = 40 * time.Second
const waitBetween = 10 * time.Second

// Cluster status when new
const StatusNew = "new"

// Cluster status when building
const StatusBuilding = "building"

// Cluster status when rebuilding swarm
const StatusRebuildingSwarm = "rebuilding-swarm"

// Does an func against a cluster then returns the new cluster representation
func (carina *WaitClusterCommand) clusterApplyWait(op clusterOp) (err error) {
	cluster, err := op(carina.ClusterName)

	if carina.Wait {
		time.Sleep(startupFudgeFactor)
		// Transitions past point of "new" or "building" are assumed to be states we
		// can stop on.
		for cluster.Status == StatusNew || cluster.Status == StatusBuilding || cluster.Status == StatusRebuildingSwarm {
			time.Sleep(waitBetween)
			cluster, err = carina.ClusterClient.Get(carina.ClusterName)
			if err != nil {
				break
			}
		}
	}

	if err != nil {
		return err
	}

	err = writeCluster(carina.TabWriter, cluster)
	if err != nil {
		return err
	}
	return carina.TabWriter.Flush()
}

// Create a cluster
func (carina *CreateCommand) Create(pc *kingpin.ParseContext) (err error) {
	return carina.clusterApplyWait(func(clusterName string) (*libcarina.Cluster, error) {
		if carina.Nodes < 1 {
			return nil, errors.New("nodes must be >= 1")
		}
		nodes := libcarina.Number(carina.Nodes)

		c := libcarina.Cluster{
			ClusterName: carina.ClusterName,
			Nodes:       nodes,
			AutoScale:   carina.AutoScale,
		}
		return carina.ClusterClient.Create(c)
	})
}

// Download credentials for a cluster
func (carina *CredentialsCommand) Download(pc *kingpin.ParseContext) (err error) {
	credentials, err := carina.ClusterClient.GetCredentials(carina.ClusterName)
	if err != nil {
		return err
	}

	if carina.Path == "" {
		carina.Path = carina.ClusterName
	}

	p := path.Clean(carina.Path)

	if p != "." {
		err = os.MkdirAll(p, 0777)
	}

	if err != nil {
		return err
	}

	err = writeCredentials(carina.TabWriter, credentials, p)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, sourceHelpString(p, os.Args[0]))

	err = carina.TabWriter.Flush()
	return err
}

func writeCredentials(w *tabwriter.Writer, creds *libcarina.Credentials, pth string) (err error) {
	// TODO: Prompt when file already exists?
	for fname, b := range creds.Files {
		p := path.Join(pth, fname)
		err = ioutil.WriteFile(p, b, 0600)
		if err != nil {
			return err
		}
	}

	return nil
}

func writeCluster(w *tabwriter.Writer, cluster *libcarina.Cluster) (err error) {
	s := strings.Join([]string{
		cluster.ClusterName,
		cluster.Flavor,
		strconv.FormatInt(cluster.Nodes.Int64(), 10),
		strconv.FormatBool(cluster.AutoScale),
		cluster.Status,
	}, "\t")
	_, err = w.Write([]byte(s + "\n"))
	return
}

func writeClusterHeader(w *tabwriter.Writer) (err error) {
	headerFields := []string{
		"ClusterName",
		"Flavor",
		"Nodes",
		"AutoScale",
		"Status",
	}
	s := strings.Join(headerFields, "\t")

	_, err = w.Write([]byte(s + "\n"))
	return err
}

func (app *Application) generateBashCompletion(c *kingpin.ParseContext) error {
	app.Writer(os.Stdout)
	if err := app.UsageForContextWithTemplate(c, 2, BashCompletionTemplate); err != nil {
		return err
	}
	return nil
}

func main() {
	app := New()
	kingpin.MustParse(app.Parse(os.Args[1:]))
}
