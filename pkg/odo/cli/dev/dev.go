package dev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	scontext "github.com/redhat-developer/odo/pkg/segment/context"

	"github.com/devfile/api/v2/pkg/apis/workspaces/v1alpha2"
	"github.com/devfile/library/pkg/devfile/parser"
	parsercommon "github.com/devfile/library/pkg/devfile/parser/data/v2/common"
	"github.com/spf13/cobra"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/redhat-developer/odo/pkg/component"
	ododevfile "github.com/redhat-developer/odo/pkg/devfile"
	"github.com/redhat-developer/odo/pkg/devfile/adapters"
	"github.com/redhat-developer/odo/pkg/devfile/adapters/common"
	"github.com/redhat-developer/odo/pkg/devfile/adapters/kubernetes"
	"github.com/redhat-developer/odo/pkg/devfile/location"
	"github.com/redhat-developer/odo/pkg/envinfo"
	"github.com/redhat-developer/odo/pkg/libdevfile"
	"github.com/redhat-developer/odo/pkg/log"
	"github.com/redhat-developer/odo/pkg/odo/cmdline"
	"github.com/redhat-developer/odo/pkg/odo/genericclioptions"
	"github.com/redhat-developer/odo/pkg/odo/genericclioptions/clientset"
	odoutil "github.com/redhat-developer/odo/pkg/odo/util"
	"github.com/redhat-developer/odo/pkg/util"
	"github.com/redhat-developer/odo/pkg/version"
	"github.com/redhat-developer/odo/pkg/watch"
)

// RecommendedCommandName is the recommended command name
const RecommendedCommandName = "dev"

type DevOptions struct {
	// Context
	*genericclioptions.Context

	// Clients
	clientset *clientset.Clientset

	// Variables
	ignorePaths []string
	out         io.Writer
	errOut      io.Writer
	// it's called "initial" because it has to be set only once when running odo dev for the first time
	// it is used to compare with updated devfile when we watch the contextDir for changes
	initialDevfileObj parser.DevfileObj
	// ctx is used to communicate with WatchAndPush to stop watching and start cleaning up
	ctx context.Context
	// cancel function ensures that any function/method listening on ctx.Done channel stops doing its work
	cancel context.CancelFunc

	// working directory
	contextDir string

	// Flags
	randomPorts bool
}

type Handler struct{}

func NewDevOptions() *DevOptions {
	return &DevOptions{
		out:    log.GetStdout(),
		errOut: log.GetStderr(),
	}
}

var devExample = templates.Examples(`
	# Deploy component to the development cluster
	%[1]s
`)

func (o *DevOptions) SetClientset(clientset *clientset.Clientset) {
	o.clientset = clientset
}

func (o *DevOptions) Complete(cmdline cmdline.Cmdline, args []string) error {
	var err error

	// Define this first so that if user hits Ctrl+c very soon after running odo dev, odo doesn't panic
	o.ctx, o.cancel = context.WithCancel(context.Background())

	o.contextDir, err = os.Getwd()
	if err != nil {
		return err
	}

	isEmptyDir, err := location.DirIsEmpty(o.clientset.FS, o.contextDir)
	if err != nil {
		return err
	}
	if isEmptyDir {
		return errors.New("this command cannot run in an empty directory, run the command in a directory containing source code or initialize using 'odo init'")
	}
	initFlags := o.clientset.InitClient.GetFlags(cmdline.GetFlags())
	err = o.clientset.InitClient.InitDevfile(initFlags, o.contextDir,
		func(interactiveMode bool) {
			scontext.SetInteractive(cmdline.Context(), interactiveMode)
			if interactiveMode {
				fmt.Println("The current directory already contains source code. " +
					"odo will try to autodetect the language and project type in order to select the best suited Devfile for your project.")
			}
		},
		func(newDevfileObj parser.DevfileObj) error {
			return newDevfileObj.WriteYamlDevfile()
		})
	if err != nil {
		return err
	}

	o.Context, err = genericclioptions.New(genericclioptions.NewCreateParameters(cmdline).NeedDevfile(""))
	if err != nil {
		return fmt.Errorf("unable to create context: %v", err)
	}

	envfileinfo, err := envinfo.NewEnvSpecificInfo("")
	if err != nil {
		return fmt.Errorf("unable to retrieve configuration information: %v", err)
	}

	o.initialDevfileObj = o.Context.EnvSpecificInfo.GetDevfileObj()

	if !envfileinfo.Exists() {
		// if env.yaml doesn't exist, get component name from the devfile.yaml
		var cmpName string
		cmpName, err = component.GatherName(o.EnvSpecificInfo.GetDevfileObj(), o.GetDevfilePath())
		if err != nil {
			return fmt.Errorf("unable to retrieve component name: %w", err)
		}
		// create env.yaml file with component, project/namespace and application info
		// TODO - store only namespace into env.yaml, we don't want to track component or application name via env.yaml
		err = envfileinfo.SetComponentSettings(envinfo.ComponentSettings{Name: cmpName, Project: o.GetProject(), AppName: "app"})
		if err != nil {
			return fmt.Errorf("failed to write new env.yaml file: %w", err)
		}
	} else if envfileinfo.GetComponentSettings().Project != o.GetProject() {
		// set namespace if the evn.yaml exists; that's the only piece we care about in env.yaml
		err = envfileinfo.SetConfiguration("project", o.GetProject())
		if err != nil {
			return fmt.Errorf("failed to update project in env.yaml file: %w", err)
		}
	}
	o.clientset.KubernetesClient.SetNamespace(o.GetProject())

	// 3 steps to evaluate the paths to be ignored when "watching" the pwd/cwd for changes
	// 1. create an empty string slice to which paths like .gitignore, .odo/odo-file-index.json, etc. will be added
	var ignores []string
	err = genericclioptions.ApplyIgnore(&ignores, "")
	if err != nil {
		return err
	}
	o.ignorePaths = ignores

	return nil
}

func (o *DevOptions) Validate() error {
	var err error
	return err
}

func (o *DevOptions) Run(ctx context.Context) error {
	var err error
	var platformContext = kubernetes.KubernetesContext{
		Namespace: o.Context.GetProject(),
	}
	var path = filepath.Dir(o.Context.EnvSpecificInfo.GetDevfilePath())
	devfileName := o.EnvSpecificInfo.GetDevfileObj().GetMetadataName()
	namespace := o.GetProject()

	// Output what the command is doing / information
	log.Title("Developing using the "+devfileName+" Devfile",
		"Namespace: "+namespace,
		"odo version: "+version.VERSION)

	log.Section("Deploying to the cluster in developer mode")
	err = o.clientset.DevClient.Start(o.Context.EnvSpecificInfo.GetDevfileObj(), platformContext, o.ignorePaths, path)
	if err != nil {
		return err
	}

	// get the endpoint/port information for containers in devfile and setup port-forwarding
	containers, err := o.Context.EnvSpecificInfo.GetDevfileObj().Data.GetComponents(parsercommon.DevfileOptions{
		ComponentOptions: parsercommon.ComponentOptions{ComponentType: v1alpha2.ContainerComponentType},
	})
	if err != nil {
		return err
	}
	ceMapping := libdevfile.GetContainerEndpointMapping(containers)
	var portPairs map[string][]string
	if o.randomPorts {
		portPairs = randomPortPairsFromContainerEndpoints(ceMapping)
	} else {
		portPairs = portPairsFromContainerEndpoints(ceMapping)
	}
	var portPairsSlice []string
	for _, v1 := range portPairs {
		portPairsSlice = append(portPairsSlice, v1...)
	}
	pod, err := o.clientset.KubernetesClient.GetPodUsingComponentName(o.Context.EnvSpecificInfo.GetDevfileObj().GetMetadataName())
	if err != nil {
		return err
	}

	// Output that the application is running, and then show the port-forwarding information
	log.Info("\nYour application is now running on the cluster")

	portsBuf := NewPortWriter(log.GetStdout(), len(portPairsSlice))
	go func() {
		err = o.clientset.KubernetesClient.SetupPortForwarding(pod, portPairsSlice, portsBuf, o.errOut)
		if err != nil {
			fmt.Printf("failed to setup port-forwarding: %v\n", err)
		}
	}()

	portsBuf.Wait()

	devFileObj := o.Context.EnvSpecificInfo.GetDevfileObj()

	scontext.SetComponentType(ctx, component.GetComponentTypeFromDevfileMetadata(devFileObj.Data.GetMetadata()))
	scontext.SetLanguage(ctx, devFileObj.Data.GetMetadata().Language)
	scontext.SetProjectType(ctx, devFileObj.Data.GetMetadata().ProjectType)
	scontext.SetDevfileName(ctx, devFileObj.GetMetadataName())

	d := Handler{}
	err = o.clientset.DevClient.Watch(devFileObj, path, o.ignorePaths, o.out, &d, o.ctx)

	return err
}

// RegenerateAdapterAndPush regenerates the adapter and pushes the files to remote pod
func (o *Handler) RegenerateAdapterAndPush(pushParams common.PushParameters, watchParams watch.WatchParameters) error {
	var adapter common.ComponentAdapter

	adapter, err := regenerateComponentAdapterFromWatchParams(watchParams)
	if err != nil {
		return fmt.Errorf("unable to generate component from watch parameters: %w", err)
	}

	err = adapter.Push(pushParams)
	if err != nil {
		return fmt.Errorf("watch command was unable to push component: %w", err)
	}

	return nil
}

func regenerateComponentAdapterFromWatchParams(parameters watch.WatchParameters) (common.ComponentAdapter, error) {
	devObj, err := ododevfile.ParseAndValidateFromFile(location.DevfileLocation(""))
	if err != nil {
		return nil, err
	}

	if !reflect.DeepEqual(parameters.InitialDevfileObj, devObj) {
		log.Warningf("devfile.yaml has been changed; please restart the `odo dev` command\n\n")
	}

	platformContext := kubernetes.KubernetesContext{
		Namespace: parameters.EnvSpecificInfo.GetNamespace(),
	}

	return adapters.NewComponentAdapter(parameters.ComponentName, parameters.Path, parameters.ApplicationName, devObj, platformContext)
}

func (o *DevOptions) HandleSignal() error {
	fmt.Fprintf(o.out, "\n\nCancelling deployment.\nThis is non-preemptive operation, it will wait for other tasks to finish first\n\n")
	o.cancel()
	// At this point, `ctx.Done()` will be raised, and the cleanup will be done
	// wait for the cleanup to finish and let the main thread finish instead of signal handler go routine from runnable
	select {}
}

// NewCmdDev implements the odo dev command
func NewCmdDev(name, fullName string) *cobra.Command {
	o := NewDevOptions()
	devCmd := &cobra.Command{
		Use:   name,
		Short: "Deploy component to development cluster",
		Long: `odo dev is a long running command that will automatically sync your source to the cluster.
It forwards endpoints with exposure values 'public' or 'internal' to a port on localhost.`,
		Example: fmt.Sprintf(devExample, fullName),
		Args:    cobra.MaximumNArgs(0),
		Run: func(cmd *cobra.Command, args []string) {
			genericclioptions.GenericRun(o, cmd, args)
		},
	}
	devCmd.Flags().BoolVarP(&o.randomPorts, "random-ports", "f", false, "Assign random ports to redirected ports")

	clientset.Add(devCmd, clientset.DEV, clientset.INIT, clientset.KUBERNETES)
	// Add a defined annotation in order to appear in the help menu
	devCmd.Annotations["command"] = "main"
	devCmd.SetUsageTemplate(odoutil.CmdUsageTemplate)

	return devCmd
}

// portPairsFromContainerEndpoints assigns a port on localhost to each port in the provided containerEndpoints map
// it returns a map of the format "<container-name>":{"<local-port-1>:<remote-port-1>", "<local-port-2>:<remote-port-2>"}
// "container1": {"400001:3000", "400002:3001"}
func portPairsFromContainerEndpoints(ceMap map[string][]int) map[string][]string {
	portPairs := make(map[string][]string)
	port := 40000

	for name, ports := range ceMap {
		for _, p := range ports {
			port++
			for {
				isPortFree := util.IsPortFree(port)
				if isPortFree {
					pair := fmt.Sprintf("%d:%d", port, p)
					portPairs[name] = append(portPairs[name], pair)
					break
				}
				port++
			}
		}
	}
	return portPairs
}

// randomPortPairsFromContainerEndpoints assigns a random (empty) port on localhost to each port in the provided containerEndpoints map
// it returns a map of the format "<container-name>":{"<local-port-1>:<remote-port-1>", "<local-port-2>:<remote-port-2>"}
// "container1": {":3000", ":3001"}
func randomPortPairsFromContainerEndpoints(ceMap map[string][]int) map[string][]string {
	portPairs := make(map[string][]string)

	for name, ports := range ceMap {
		for _, p := range ports {
			pair := fmt.Sprintf(":%d", p)
			portPairs[name] = append(portPairs[name], pair)
		}
	}
	return portPairs
}
