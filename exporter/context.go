package exporter

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/databricks/databricks-sdk-go"
	"github.com/databricks/databricks-sdk-go/service/catalog"
	"github.com/databricks/databricks-sdk-go/service/compute"
	"golang.org/x/exp/maps"

	"github.com/databricks/terraform-provider-databricks/commands"
	"github.com/databricks/terraform-provider-databricks/common"
	"github.com/databricks/terraform-provider-databricks/provider"
	"github.com/databricks/terraform-provider-databricks/scim"
	"github.com/databricks/terraform-provider-databricks/workspace"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/zclconf/go-cty/cty"
)

/** High level overview of importer design:

                                    +----------+  Add  +--------------------+
                                    | resource +-------> stateApproximation |
                                    +--^-------+       +----|-------------|-+
                                       |                    |             |
  +------------------------+           | Emit               |             |
  | "normal provider flow" |    +------^-----+        +-----V-----+   +---V---+
  +------------^-----------+    | importable +--------> reference |   | scope |
               |                +------^-----+        +--------|--+   +---V---+
+--------------+--------------+        |                       |          |
|terraform-provider-databricks|        |                       |          |
+--------------+--------------+        |                       |          |
               |                       | List               +--V----------V---+
    +----------v---------+        +----^------------+       |                 |
    |                    |        |                 |       |    Generated    |
    |  importer command  +-------->  importContext  |       |    HCL Files    |
    |                    |        |                 |       |                 |
    +--------------------+        +-----------------+       +-----------------+
*/

type resourceChannel chan *resource

type importContext struct {
	// not modified/used only in single thread
	Module            string
	Context           context.Context
	Client            *common.DatabricksClient
	Importables       map[string]importable
	Resources         map[string]*schema.Resource
	Directory         string
	nameFixes         []regexFix
	hclFixes          []regexFix
	variables         map[string]string
	workspaceConfKeys map[string]any

	workspaceClient *databricks.WorkspaceClient
	accountClient   *databricks.AccountClient

	channels                 map[string]resourceChannel
	defaultChannel           resourceChannel
	defaultHanlerChannelSize int

	// mutable resources
	State *stateApproximation
	Scope importedResources

	// command-line resources (immutable, or set by the single thread)
	includeUserDomains       bool
	importAllUsers           bool
	exportDeletedUsersAssets bool
	incremental              bool
	mounts                   bool
	noFormat                 bool
	services                 map[string]struct{}
	listing                  string
	match                    string
	lastActiveDays           int64
	lastActiveMs             int64
	generateDeclaration      bool
	meAdmin                  bool
	prefix                   string
	accountLevel             bool
	shImports                map[string]bool
	notebooksFormat          string
	updatedSinceStr          string
	updatedSinceMs           int64

	waitGroup *sync.WaitGroup

	// TODO: protect by mutex?
	mountMap map[string]mount

	//
	testEmits      map[string]bool
	testEmitsMutex sync.Mutex

	//
	allGroups   []scim.Group
	groupsMutex sync.Mutex

	//
	allUsers        map[string]scim.User
	usersMutex      sync.RWMutex
	allUsersMapping map[string]string // maps user_name -> internal ID
	allUsersMutex   sync.RWMutex

	//
	allSps        map[string]scim.User
	allSpsMapping map[string]string // maps application_id -> internal ID
	spsMutex      sync.RWMutex

	//
	importing      map[string]bool
	importingMutex sync.RWMutex

	//
	sqlDatasources      map[string]string
	sqlDatasourcesMutex sync.Mutex

	// workspace-related objects & corresponding mutex
	allDirectories      []workspace.ObjectStatus
	allWorkspaceObjects []workspace.ObjectStatus
	wsObjectsMutex      sync.RWMutex

	builtInPolicies      map[string]compute.PolicyFamily
	builtInPoliciesMutex sync.Mutex

	// Workspace-level UC Metastore information
	currentMetastore *catalog.GetMetastoreSummaryResponse

	// tracking ignored objects
	ignoredResourcesMutex sync.Mutex
	ignoredResources      map[string]struct{}

	// emitting of users/SPs
	emittedUsers      map[string]struct{}
	emittedUsersMutex sync.RWMutex

	//
	userOrSpDirectories      map[string]bool
	userOrSpDirectoriesMutex sync.RWMutex
}

type mount struct {
	URL             string
	InstanceProfile string
	ClusterID       string
}

var nameFixes = []regexFix{
	{regexp.MustCompile(`[0-9a-f]{8}[_-][0-9a-f]{4}[_-][0-9a-f]{4}` +
		`[_-][0-9a-f]{4}[_-][0-9a-f]{12}[_-]`), ""},
	{regexp.MustCompile(`[-\s\.\|]`), "_"},
	{regexp.MustCompile(`\W+`), "_"},
	{regexp.MustCompile(`[_]{2,}`), "_"},
}

// less aggressive name normalizations
var simpleNameFixes = []regexFix{
	{nameNormalizationRegex, "_"},
}

var workspaceConfKeys = map[string]any{
	"enableIpAccessLists":                              false,
	"enableTokensConfig":                               false,
	"maxTokenLifetimeDays":                             0,
	"maxUserInactiveDays":                              0,
	"storeInteractiveNotebookResultsInCustomerAccount": false,
	"enableDeprecatedClusterNamedInitScripts":          false,
	"enableDeprecatedGlobalInitScripts":                false,
}

const (
	defaultChannelSize = 100000
	defaultNumRoutines = 2
	envVariablePrefix  = "EXPORTER_PARALLELISM_"
)

// increased concurrency limits, could be also overridden via environment variables with name: envVariablePrefix + resource type
var goroutinesNumber = map[string]int{
	"databricks_notebook":          10,
	"databricks_directory":         5,
	"databricks_workspace_file":    5,
	"databricks_dbfs_file":         3,
	"databricks_user":              1,
	"databricks_service_principal": 1,
	"databricks_sql_dashboard":     3,
	"databricks_sql_widget":        4,
	"databricks_sql_visualization": 4,
	"databricks_sql_query":         5,
	"databricks_sql_alert":         2,
	"databricks_permissions":       11,
}

func makeResourcesChannels() map[string]resourceChannel {
	resources := []string{"databricks_user", "databricks_service_principal", "databricks_group"}
	if val, exists := os.LookupEnv("EXPORTER_DEDICATED_RESOUSE_CHANNELS"); exists {
		resources = strings.Split(val, ",")
	}
	channels := make(map[string]resourceChannel, len(resources))
	for _, r := range resources {
		channels[r] = make(resourceChannel, defaultChannelSize)
	}
	return channels
}

func newImportContext(c *common.DatabricksClient) *importContext {
	p := provider.DatabricksProvider()
	p.TerraformVersion = "exporter"
	p.SetMeta(c)
	ctx := context.WithValue(context.Background(), common.Provider, p)
	ctx = context.WithValue(ctx, common.ResourceName, "exporter")
	c.WithCommandExecutor(func(
		ctx context.Context,
		c *common.DatabricksClient) common.CommandExecutor {
		return commands.NewCommandsAPI(ctx, c)
	})

	defaultHanlerChannelSize := getEnvAsInt("EXPORTER_DEFAULT_HANDLER_CHANNEL_SIZE", defaultChannelSize*3)

	supportedResources := maps.Keys(resourcesMap)
	return &importContext{
		Client:                   c,
		Context:                  ctx,
		State:                    newStateApproximation(supportedResources),
		Importables:              resourcesMap,
		Resources:                p.ResourcesMap,
		Scope:                    importedResources{},
		importing:                map[string]bool{},
		nameFixes:                nameFixes,
		hclFixes:                 []regexFix{}, // Be careful with that! it may break working code
		variables:                map[string]string{},
		allDirectories:           []workspace.ObjectStatus{},
		allWorkspaceObjects:      []workspace.ObjectStatus{},
		workspaceConfKeys:        workspaceConfKeys,
		shImports:                map[string]bool{},
		notebooksFormat:          "SOURCE",
		allUsers:                 map[string]scim.User{},
		allSps:                   map[string]scim.User{},
		waitGroup:                &sync.WaitGroup{},
		channels:                 makeResourcesChannels(),
		defaultHanlerChannelSize: defaultHanlerChannelSize,
		defaultChannel:           make(resourceChannel, defaultHanlerChannelSize),
		ignoredResources:         map[string]struct{}{},
		emittedUsers:             map[string]struct{}{},
		userOrSpDirectories:      map[string]bool{},
	}
}

func getLastRunString(fileName string) string {
	var updatedSinceStr string
	statsData, err := os.ReadFile(fileName)
	if err == nil {
		var m map[string]any
		err = json.Unmarshal(statsData, &m)
		s, exists := m["startTime"]
		if err == nil && exists {
			updatedSinceStr = s.(string)
		} else {
			log.Printf("[WARN] Can't decode data: err=%v or startTime field doesn't exist. data: '%s'",
				err, string(statsData))
		}
	} else {
		log.Printf("[WARN] Can't load data from file %s: %v", fileName, err)
	}
	return updatedSinceStr
}

func (ic *importContext) Run() error {
	startTime := time.Now()
	statsFileName := ic.Directory + "/exporter-run-stats.json"
	if len(ic.services) == 0 {
		return fmt.Errorf("no services to import")
	}

	if ic.incremental {
		if ic.updatedSinceStr == "" {
			ic.updatedSinceStr = getLastRunString(statsFileName)
		}
		if ic.updatedSinceStr == "" {
			return fmt.Errorf("-updated-since is required with -interactive parameter if %s file doesn't exist",
				statsFileName)
		}
		tm, err := time.Parse(time.RFC3339, ic.updatedSinceStr)
		if err != nil {
			return fmt.Errorf("can't parse value '%s' please specify it in ISO8601 format, i.e. 2023-07-01T00:00:00Z",
				ic.updatedSinceStr)
		}
		ic.updatedSinceStr = tm.UTC().Format(time.RFC3339)
		tm, _ = time.Parse(time.RFC3339, ic.updatedSinceStr)
		ic.updatedSinceMs = tm.UnixMilli()
	}

	log.Printf("[INFO] Importing %s module into %s directory Databricks resources of %s services",
		ic.Module, ic.Directory, maps.Keys(ic.services))

	ic.notebooksFormat = strings.ToUpper(ic.notebooksFormat)
	_, supportedFormat := fileExtensionFormatMapping[ic.notebooksFormat]
	if !supportedFormat && ic.notebooksFormat != "SOURCE" {
		return fmt.Errorf("unsupported notebook format: '%s'", ic.notebooksFormat)
	}

	info, err := os.Stat(ic.Directory)
	if os.IsNotExist(err) {
		err = os.MkdirAll(ic.Directory, 0755)
		if err != nil {
			return fmt.Errorf("can't create directory %s", ic.Directory)
		}
	} else if !info.IsDir() {
		return fmt.Errorf("the path %s is not a directory", ic.Directory)
	}

	ic.accountLevel = ic.Client.Config.IsAccountClient()
	if ic.accountLevel {
		ic.meAdmin = true
		ic.accountClient, err = ic.Client.AccountClient()
		if err != nil {
			return err
		}
	} else {
		ic.workspaceClient, err = ic.Client.WorkspaceClient()
		if err != nil {
			return err
		}
		me, err := ic.workspaceClient.CurrentUser.Me(ic.Context)
		if err != nil {
			return err
		}
		for _, g := range me.Groups {
			if g.Display == "admins" {
				ic.meAdmin = true
				break
			}
		}
		currentMetastore, err := ic.workspaceClient.Metastores.Summary(ic.Context)
		if err == nil {
			ic.currentMetastore = currentMetastore
		} else {
			log.Printf("[WARN] can't get current UC metastore: %v", err)
		}
	}
	// Concurrent execution part
	if ic.waitGroup == nil {
		ic.waitGroup = &sync.WaitGroup{}
	}
	// Start goroutines for each resource type
	ic.startImportChannels()

	// Start listing of objects
	for rnLoop, irLoop := range ic.Importables {
		resourceName := rnLoop
		ir := irLoop
		if ir.List == nil {
			continue
		}
		if !strings.Contains(ic.listing, ir.Service) {
			log.Printf("[DEBUG] %s (%s service) is not part of listing", resourceName, ir.Service)
			continue
		}
		if ic.accountLevel && !ir.AccountLevel {
			log.Printf("[DEBUG] %s (%s service) is not a account level resource", resourceName, ir.Service)
			continue
		}
		if !ic.accountLevel && !ir.WorkspaceLevel {
			log.Printf("[DEBUG] %s (%s service) is not a workspace level resource", resourceName, ir.Service)
			continue
		}
		ic.waitGroup.Add(1)
		go func() {
			if err := ir.List(ic); err != nil {
				log.Printf("[ERROR] %s (%s service) listing failed: %s", resourceName, ir.Service, err)
			}
			log.Printf("[DEBUG] Finished listing for service %s", resourceName)
			ic.waitGroup.Done()
		}()
	}

	ic.waitGroup.Wait()
	// close channels
	ic.closeImportChannels()

	// This should be single threaded...
	if ic.Scope.Len() == 0 {
		return fmt.Errorf("no resources to import")
	}
	shFileName := fmt.Sprintf("%s/import.sh", ic.Directory)
	if ic.incremental {
		shFile, err := os.Open(shFileName)
		if err == nil {
			defer shFile.Close()
			fileScanner := bufio.NewScanner(shFile)
			fileScanner.Split(bufio.ScanLines)
			for fileScanner.Scan() {
				line := fileScanner.Text()
				if strings.HasPrefix(line, "terraform import ") {
					ic.shImports[strings.TrimRight(line, "\n")] = true
				}
			}
		} else {
			log.Printf("[ERROR] opening %s: %v", shFileName, err)
		}
	}
	sh, err := os.OpenFile(shFileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer sh.Close()
	// nolint
	sh.WriteString("#!/bin/sh\n\nset -e\n\n")

	if ic.generateDeclaration {
		dcfile, err := os.Create(fmt.Sprintf("%s/databricks.tf", ic.Directory))
		if err != nil {
			return err
		}
		// nolint
		dcfile.WriteString(
			`terraform {
				required_providers {
			  		databricks = {
						source  = "databricks/databricks"
						version = "` + common.Version() + `"
				  	}
				}
		  	}

		  	provider "databricks" {
		  	`)
		if ic.accountLevel {
			dcfile.WriteString(fmt.Sprintf(`	host       = "%s"
				account_id = "%s"
			`, ic.Client.Config.Host, ic.Client.Config.AccountID))
		}
		dcfile.WriteString(`}`)
		dcfile.Close()
	}
	//
	ic.generateAndWriteResources(sh)
	err = ic.generateVariables()
	if err != nil {
		return err
	}

	//
	if stats, err := os.Create(statsFileName); err == nil {
		defer stats.Close()
		statsData := map[string]any{
			"startTime":       startTime.UTC().Format(time.RFC3339),
			"duration":        fmt.Sprintf("%f sec", time.Since(startTime).Seconds()),
			"exportedObjects": ic.Scope.Len(),
		}
		statsBytes, _ := json.Marshal(statsData)
		if _, err = stats.Write(statsBytes); err != nil {
			return err
		}
	}

	// output ignored resources...
	if ignored, err := os.Create(fmt.Sprintf("%s/ignored_resources.txt", ic.Directory)); err == nil {
		defer ignored.Close()
		ic.ignoredResourcesMutex.Lock()
		keys := maps.Keys(ic.ignoredResources)
		sort.Strings(keys)
		for _, s := range keys {
			ignored.WriteString(s + "\n")
		}
	}

	if !ic.noFormat {
		// format generated source code
		cmd := exec.CommandContext(context.Background(), "terraform", "fmt")
		cmd.Dir = ic.Directory
		err = cmd.Run()
		if err != nil {
			log.Printf("[ERROR] problems when formatting the generated code: %v", err)
			return err
		}
	}
	log.Printf("[INFO] Done. Please edit the files and roll out new environment.")
	return nil
}

func (ic *importContext) resourceHandler(num int, resourceType string, ch resourceChannel) {
	log.Printf("[DEBUG] Starting goroutine %d for resource %s", num, resourceType)
	for r := range ch {
		log.Printf("[DEBUG] channel for %s, channel size=%d got %v", resourceType, len(ch), r)
		if r != nil {
			r.ImportResource(ic)
			log.Printf("[DEBUG] Finished importing %s, %v", resourceType, r)
		}
	}
}

func (ic *importContext) startImportChannels() {
	for rt, c := range ic.channels {
		ch := c
		resourceType := rt
		numRoutines, exists := goroutinesNumber[resourceType]
		if !exists {
			numRoutines = defaultNumRoutines
		}
		numRoutines = getEnvAsInt(envVariablePrefix+resourceType, numRoutines)

		for i := 0; i < numRoutines; i++ {
			num := i
			go func() {
				ic.resourceHandler(num, resourceType, ch)
			}()
		}
	}

	numRoutines := getEnvAsInt(envVariablePrefix+"default", 15)
	for i := 0; i < numRoutines; i++ {
		num := i
		go func() {
			ic.resourceHandler(num, "default", ic.defaultChannel)
		}()
	}
}

func (ic *importContext) closeImportChannels() {
	for rt, ch := range ic.channels {
		log.Printf("[DEBUG] Closing channel for resource %s", rt)
		close(ch)
	}
	close(ic.defaultChannel)
}

func generateBlockFullName(block *hclwrite.Block) string {
	return block.Type() + "_" + strings.Join(block.Labels(), "_")
}

type resourceWriteData struct {
	BlockName     string
	ResourceBody  string
	ImportCommand string
}

type dataWriteChannel chan *resourceWriteData
type importWriteChannel chan string

func (ic *importContext) handleResourceWrite(generatedFile string, ch dataWriteChannel, importChan importWriteChannel) {
	var existingFile *hclwrite.File
	if ic.incremental {
		log.Printf("[DEBUG] Going to read existing file %s", generatedFile)
		content, err := os.ReadFile(generatedFile)
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("[WARN] File %s doesn't exist when using incremental export", generatedFile)
		} else if err != nil {
			log.Printf("[ERROR] error opening %s", generatedFile)
		} else {
			log.Printf("[DEBUG] Going to parse existing file %s", generatedFile)
			var diags hcl.Diagnostics
			existingFile, diags = hclwrite.ParseConfig(content, generatedFile, hcl.Pos{Line: 1, Column: 1})
			if diags.HasErrors() {
				log.Printf("[ERROR] parsing of existing file %s failed: %s", generatedFile, diags.Error())
			}
		}
	}
	if existingFile == nil {
		existingFile = hclwrite.NewEmptyFile()
	}

	tf, err := os.Create(generatedFile)
	if err != nil {
		log.Printf("[ERROR] Can't create %s: %v", generatedFile, err)
		return
	}

	//
	newResources := make(map[string]struct{}, 100)
	for f := range ch {
		if f != nil {
			log.Printf("[DEBUG] started writing resource body for %s", f.BlockName)
			_, err = tf.WriteString(f.ResourceBody)
			if err == nil {
				newResources[f.BlockName] = struct{}{}
				if f.ImportCommand != "" {
					ic.waitGroup.Add(1)
					importChan <- f.ImportCommand
				}
				log.Printf("[DEBUG] finished writing resource body for %s", f.BlockName)
			} else {
				log.Printf("[ERROR] Error when writing to %s: %v", generatedFile, err)
			}
		} else {
			log.Print("[WARN] got nil as resourceWriteData!")
		}
		ic.waitGroup.Done()
	}
	// update existing file if incremental mode
	numResources := len(newResources)
	if ic.incremental {
		log.Printf("[DEBUG] Starting to merge existing resources for %s", generatedFile)
		f := hclwrite.NewEmptyFile()
		for _, block := range existingFile.Body().Blocks() {
			blockName := generateBlockFullName(block)
			_, exists := newResources[blockName]
			if exists {
				log.Printf("[DEBUG] resource %s already generated, skipping...", blockName)
			} else {
				log.Printf("[DEBUG] resource %s doesn't exist, adding...", blockName)
				f.Body().AppendBlock(block)
				numResources = numResources + 1
			}
		}
		_, err = tf.WriteString(string(f.Bytes()))
		if err != nil {
			log.Printf("[ERROR] error when writing existing resources for file %s: %v", generatedFile, err)
		}
		log.Printf("[DEBUG] Finished merging existing resources for %s", generatedFile)
	}
	tf.Close()
	if numResources == 0 {
		log.Printf("[DEBUG] removing empty file %s - no resources for a given service", generatedFile)
		os.Remove(generatedFile)
	}
}

func (ic *importContext) writeImports(sh *os.File, importChan importWriteChannel) {
	for importCommand := range importChan {
		if importCommand != "" && sh != nil {
			log.Printf("[DEBUG] writing import command %s", importCommand)
			sh.WriteString(importCommand + "\n")
			delete(ic.shImports, importCommand)
		} else {
			log.Print("[WARN] got empty import command... or file is nil")
		}
		ic.waitGroup.Done()
	}
	if sh != nil {
		log.Printf("[DEBUG] Writing the rest of import commands. len=%d", len(ic.shImports))
		for k := range ic.shImports {
			sh.WriteString(k + "\n")
		}
	}
}

func (ic *importContext) processSingleResource(resourcesChan resourceChannel, writerChannels map[string]dataWriteChannel) {
	processed := 0
	generated := 0
	ignored := 0
	for r := range resourcesChan {
		processed = processed + 1
		if r == nil {
			log.Print("[WARN] Got nil resource...")
			ic.waitGroup.Done()
			continue
		}
		ir := ic.Importables[r.Resource]
		if ir.Ignore != nil && ir.Ignore(ic, r) {
			log.Printf("[WARN] Ignoring resource %s: %s", r.Resource, r.Name)
			ignored = ignored + 1
			ic.waitGroup.Done()
			continue
		}
		var err error
		f := hclwrite.NewEmptyFile()
		log.Printf("[TRACE] Generating %s: %s", r.Resource, r.Name)
		body := f.Body()
		if ir.Body != nil {
			err = ir.Body(ic, body, r)
			if err != nil {
				log.Printf("[ERROR] error calling ir.Body for %v: %s", r, err.Error())
			}
		} else {
			resourceBlock := body.AppendNewBlock("resource", []string{r.Resource, r.Name})
			err = ic.dataToHcl(ir, []string{}, ic.Resources[r.Resource],
				r.Data, resourceBlock.Body())
			if err != nil {
				log.Printf("[ERROR] error generating body for %v: %s", r, err.Error())
			}
		}
		if err == nil && len(body.Blocks()) > 0 {
			formatted := hclwrite.Format(f.Bytes())
			// fix some formatting in a hacky way instead of writing 100 lines of HCL AST writer code
			formatted = []byte(ic.regexFix(string(formatted), ic.hclFixes))
			writeData := &resourceWriteData{
				ResourceBody: string(formatted),
				BlockName:    generateBlockFullName(body.Blocks()[0]),
			}
			if r.Mode != "data" && ic.Resources[r.Resource].Importer != nil {
				writeData.ImportCommand = r.ImportCommand(ic)
			}
			ch, exists := writerChannels[ir.Service]
			if exists {
				ic.waitGroup.Add(1)
				ch <- writeData
			} else {
				log.Printf("[WARN] can't find a channel for service: %s, resource: %s", ir.Service, r.Resource)
			}
			log.Printf("[TRACE] Finished generating %s: %s", r.Resource, r.Name)
			generated = generated + 1
		} else {
			log.Printf("[WARN] error generating resource body: %v, or body blocks len is 0", err)
		}
		ic.waitGroup.Done()
	}
	log.Printf("[DEBUG] processed resources: %d, generated: %d, ignored: %d", processed, generated, ignored)
}

func (ic *importContext) generateAndWriteResources(sh *os.File) {
	resources := ic.Scope.Sorted()
	scopeSize := ic.Scope.Len()
	t1 := time.Now()
	log.Printf("[INFO] Generating configuration for %d resources", scopeSize)

	// make configurable via environment variables
	resourceHandlersNumber := getEnvAsInt("EXPORTER_RESOURCE_GENERATORS", 50)
	resourcesChan := make(resourceChannel, defaultChannelSize)

	resourceWriters := make(map[string]dataWriteChannel, len(ic.Resources))
	for _, imp := range ic.Importables {
		resourceWriters[imp.Service] = make(dataWriteChannel, defaultChannelSize)
	}
	importChan := make(importWriteChannel, defaultChannelSize)
	//
	go func() {
		ic.writeImports(sh, importChan)
	}()
	for i := 0; i < resourceHandlersNumber; i++ {
		i := i
		go func() {
			log.Printf("[DEBUG] Starting resource handler %d", i)
			ic.processSingleResource(resourcesChan, resourceWriters)
		}()
	}
	for service, ch := range resourceWriters {
		service := service
		ch := ch
		generatedFile := fmt.Sprintf("%s/%s.tf", ic.Directory, service)
		log.Printf("[DEBUG] starting writer for service %s", service)
		go func() {
			ic.handleResourceWrite(generatedFile, ch, importChan)
		}()
	}

	//
	for i, r := range resources {
		ic.waitGroup.Add(1)
		resourcesChan <- r
		if i%500 == 0 {
			log.Printf("[INFO] Submitted %d of %d resources", i+1, scopeSize)
		}
	}
	ic.waitGroup.Wait()
	// close all channels
	close(importChan)
	close(resourcesChan)
	for service, ch := range resourceWriters {
		log.Printf("Closing writer for service %s", service)
		close(ch)
	}

	log.Printf("[INFO] Finished generation of configuration for %d resources (took %v seconds)",
		scopeSize, time.Since(t1).Seconds())
}

func (ic *importContext) generateVariables() error {
	if len(ic.variables) == 0 {
		return nil
	}
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	fileName := fmt.Sprintf("%s/vars.tf", ic.Directory)
	if ic.incremental {
		content, err := os.ReadFile(fileName)
		if err == nil {
			ftmp, diags := hclwrite.ParseConfig(content, fileName, hcl.Pos{Line: 1, Column: 1})
			if diags.HasErrors() {
				log.Printf("[ERROR] parsing of existing file failed: %s", diags)
			} else {
				tbody := ftmp.Body()
				for _, block := range tbody.Blocks() {
					typ := block.Type()
					labels := block.Labels()
					log.Printf("[DEBUG] blockBody: %v %v\n", typ, labels)
					_, present := ic.variables[labels[0]]
					if typ == "variable" && present {
						log.Printf("[DEBUG] Ignoring variable '%s' that will be re-exported", labels[0])
					} else {
						log.Printf("[DEBUG] Adding not exported object. type='%s', labels=%v", typ, labels)
						body.AppendBlock(block)
					}
				}
			}
		} else {
			log.Printf("[ERROR] opening file %s", fileName)
		}
	}
	vf, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer vf.Close()

	for k, v := range ic.variables {
		b := body.AppendNewBlock("variable", []string{k}).Body()
		b.SetAttributeValue("description", cty.StringVal(v))
	}
	// nolint
	vf.Write(f.Bytes())
	log.Printf("[INFO] Written %d variables", len(ic.variables))
	return nil
}

func (ic *importContext) MatchesName(n string) bool {
	if ic.match == "" {
		return true
	}
	return strings.Contains(strings.ToLower(n), strings.ToLower(ic.match))
}

func genTraversalTokens(sr *resourceApproximation, pick string) hcl.Traversal {
	if sr.Mode == "data" {
		return hcl.Traversal{
			hcl.TraverseRoot{Name: "data"},
			hcl.TraverseAttr{Name: sr.Type},
			hcl.TraverseAttr{Name: sr.Name},
			hcl.TraverseAttr{Name: pick},
		}
	}
	return hcl.Traversal{
		hcl.TraverseRoot{Name: sr.Type},
		hcl.TraverseAttr{Name: sr.Name},
		hcl.TraverseAttr{Name: pick},
	}
}

// this will run single threaded
func (ic *importContext) Find(r *resource, pick string, ref reference) (string, hcl.Traversal) {
	log.Printf("[DEBUG] Starting searching for reference for resource %s %s, pick=%s, ref=%v", r.Resource, r.ID, pick, ref)
	// TODO: Can we cache findings?
	// optimize performance by avoiding doing regexp matching multiple times
	matchValue := ""
	if ref.MatchType == MatchRegexp {
		if ref.Regexp == nil {
			log.Printf("[WARN] you must provide regular expression for 'regexp' match type")
			return "", nil
		}
		res := ref.Regexp.FindStringSubmatch(r.Value)
		if len(res) < 2 {
			log.Printf("[WARN] no match for regexp: %v in string %s", ref.Regexp, r.Value)
			return "", nil
		}
		matchValue = res[1]
	} else if ref.MatchType == MatchCaseInsensitive {
		matchValue = strings.ToLower(r.Value) // performance optimization to avoid doing it in the loop
	} else if ref.MatchType == MatchExact || ref.MatchType == MatchDefault {
		matchValue = r.Value
	}
	// doing explicit lookup in the state.  For case insensitive matches, first attempt to lookup for the value, and do iteration if it's not found
	if ref.MatchType == MatchExact || ref.MatchType == MatchDefault || ref.MatchType == MatchRegexp || ref.MatchType == MatchCaseInsensitive {
		sr := ic.State.Get(r.Resource, r.Attribute, matchValue)
		if sr != nil {
			log.Printf("[DEBUG] Finished direct lookup for reference for resource %s %s, pick=%s, ref=%v. Found: type=%s name=%s",
				r.Resource, r.ID, pick, ref, sr.Type, sr.Name)
			return matchValue, genTraversalTokens(sr, pick)
		}
		if ref.MatchType != MatchCaseInsensitive { // for case-insensitive matching we'll try iteration
			log.Printf("[DEBUG] Finished direct lookup for reference for resource %s %s, pick=%s, ref=%v. Not found",
				r.Resource, r.ID, pick, ref)
			return "", nil
		}
	}

	for _, sr := range *ic.State.Resources(r.Resource) {
		for _, i := range sr.Instances {
			v := i.Attributes[r.Attribute]
			if v == nil {
				log.Printf("[WARN] Can't find instance attribute '%v' in resource: '%v' with name '%v', ID: '%v'",
					r.Attribute, r.Resource, r.Name, r.ID)
				continue
			}
			strValue := v.(string)
			matched := false
			switch ref.MatchType {
			case MatchCaseInsensitive:
				matched = (strings.ToLower(strValue) == matchValue)
			case MatchPrefix:
				matched = strings.HasPrefix(r.Value, strValue)
			default:
				log.Printf("[WARN] Unsupported match type: %s", ref.MatchType)
			}
			if !matched {
				continue
			}
			// TODO: we need to not generate traversals resources for which their Ignore function returns true...
			log.Printf("[DEBUG] Finished searching for reference for resource %s %s, pick=%s, ref=%v. Found: type=%s name=%s",
				r.Resource, r.ID, pick, ref, sr.Type, sr.Name)
			return strValue, genTraversalTokens(sr, pick)
		}
	}
	log.Printf("[DEBUG] Finished searching for reference for resource %s %s, pick=%s, ref=%v. Not found", r.Resource, r.ID, pick, ref)
	return "", nil
}

// This function checks if resource exist in any state (already added or in process of addition)
func (ic *importContext) Has(r *resource) bool {
	return ic.HasInState(r, false)
}

func (ic *importContext) isImporting(s string) (bool, bool) {
	ic.importingMutex.RLocker().Lock()
	defer ic.importingMutex.RLocker().Unlock()
	v, visiting := ic.importing[s]
	return v, visiting
}

// This function checks if resource exist. onlyAdded flag enforces that true is returned only if it was added with Add()
func (ic *importContext) HasInState(r *resource, onlyAdded bool) bool {
	v, visiting := ic.isImporting(r.String())
	if visiting && (v || !onlyAdded) {
		return true
	}
	return ic.State.Has(r)
}

func (ic *importContext) setImportingState(s string, state bool) {
	ic.importingMutex.Lock()
	defer ic.importingMutex.Unlock()
	ic.importing[s] = state
}

func (ic *importContext) Add(r *resource) {
	if ic.HasInState(r, true) { // resource must exist and already marked as added
		return
	}
	ic.setImportingState(r.String(), true) // mark resource as added
	state := r.Data.State()
	if state == nil {
		log.Printf("[ERROR] state is nil for %s", r)
		return
	}
	inst := instanceApproximation{
		Attributes: map[string]any{},
	}
	for k, v := range state.Attributes {
		inst.Attributes[k] = v
	}
	if r.Mode == "" {
		r.Mode = "managed"
	}
	inst.Attributes["id"] = r.ID
	ic.State.Append(resourceApproximation{
		Mode:      r.Mode,
		Module:    ic.Module,
		Type:      r.Resource,
		Name:      r.Name,
		Instances: []instanceApproximation{inst},
	})
	// in single-threaded scenario scope is toposorted
	ic.Scope.Append(r)
}

func (ic *importContext) regexFix(s string, fixes []regexFix) string {
	for _, x := range fixes {
		s = x.Regex.ReplaceAllString(s, x.Replacement)
	}
	return s
}

func (ic *importContext) ResourceName(r *resource) string {
	name := r.Name
	if name == "" && ic.Importables[r.Resource].Name != nil {
		name = ic.Importables[r.Resource].Name(ic, r.Data)
	}
	if name == "" {
		name = r.ID
	}
	name = ic.prefix + name
	origCaseName := name
	name = strings.ToLower(name)
	name = ic.regexFix(name, ic.nameFixes)
	// this is either numeric id or all-non-ascii
	if regexp.MustCompile(`^\d`).MatchString(name) || name == "" {
		if name == "" {
			origCaseName = r.ID
		}
		name = fmt.Sprintf("r%x", md5.Sum([]byte(origCaseName)))[0:12]
	}
	return name
}

func (ic *importContext) isServiceEnabled(service string) bool {
	_, exists := ic.services[service]
	return exists
}

func (ic *importContext) Emit(r *resource) {
	// TODO: change into channels, if stack trace depth issues would surface
	_, v := r.MatchPair()
	if v == "" {
		log.Printf("[DEBUG] %s has got empty identifier", r)
		return
	}
	ir, ok := ic.Importables[r.Resource]
	if !ok {
		log.Printf("[ERROR] %s is not available for import", r)
		return
	}
	if !ic.isServiceEnabled(ir.Service) {
		log.Printf("[DEBUG] %s (%s service) is not part of the import", r.Resource, ir.Service)
		return
	}
	if ic.Has(r) {
		log.Printf("[DEBUG] %s already imported", r)
		return
	}
	if ic.testEmits != nil {
		log.Printf("[INFO] %s is emitted in test mode", r)
		ic.testEmitsMutex.Lock()
		ic.testEmits[r.String()] = true
		ic.testEmitsMutex.Unlock()
		return
	}
	ic.setImportingState(r.String(), false) // we're starting to add a new resource
	_, ok = ic.Resources[r.Resource]
	if !ok {
		log.Printf("[ERROR] %s is not available in provider", r)
		return
	}

	if ic.accountLevel && !ir.AccountLevel {
		log.Printf("[DEBUG] %s (%s service) is not part of the account level export", r.Resource, ir.Service)
		return
	}
	// TODO: add similar condition for checking workspace-level objects only. After new ACLs import is merged

	// from here, it should be done by the goroutine...  send resource into the channel
	ch, exists := ic.channels[r.Resource]
	if exists {
		log.Printf("[TRACE] increasing counter & sending to the channel for resource %s", r.Resource)
		ic.waitGroup.Add(1)
		ch <- r
	} else {
		log.Print("[TRACE] increasing counter & sending to the default channel")
		ic.waitGroup.Add(1)
		ic.defaultChannel <- r
	}
}

func maybeAddQuoteCharacter(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

func (ic *importContext) getTraversalTokens(ref reference, value string) hclwrite.Tokens {
	matchType := ref.MatchTypeValue()
	attr := ref.MatchAttribute()
	attrValue, traversal := ic.Find(&resource{
		Resource:  ref.Resource,
		Attribute: attr,
		Value:     value,
	}, attr, ref)
	// at least one invocation of ic.Find will assign Nil to traversal if resource with value is not found
	if traversal == nil {
		return nil
	}
	switch matchType {
	case MatchExact, MatchDefault, MatchCaseInsensitive:
		return hclwrite.TokensForTraversal(traversal)
	case MatchPrefix:
		rest := value[len(attrValue):]
		tokens := hclwrite.Tokens{&hclwrite.Token{Type: hclsyntax.TokenOQuote, Bytes: []byte{'"', '$', '{'}}}
		tokens = append(tokens, hclwrite.TokensForTraversal(traversal)...)
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCQuote, Bytes: []byte{'}'}})
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenQuotedLit, Bytes: []byte(maybeAddQuoteCharacter(rest))})
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCQuote, Bytes: []byte{'"'}})
		return tokens
	case MatchRegexp:
		indices := ref.Regexp.FindStringSubmatchIndex(value)
		if len(indices) == 4 {
			tokens := hclwrite.Tokens{&hclwrite.Token{Type: hclsyntax.TokenOQuote, Bytes: []byte{'"'}}}
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenQuotedLit, Bytes: []byte(maybeAddQuoteCharacter(value[0:indices[2]]))})
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenOQuote, Bytes: []byte{'$', '{'}})
			tokens = append(tokens, hclwrite.TokensForTraversal(traversal)...)
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCQuote, Bytes: []byte{'}'}})
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenQuotedLit, Bytes: []byte(maybeAddQuoteCharacter(value[indices[3]:]))})
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCQuote, Bytes: []byte{'"'}})
			return tokens
		}
		log.Printf("[WARN] Can't match found data in '%s'. Indices: %v", value, indices)
	default:
		log.Printf("[WARN] Unsupported match type: %s", ref.MatchType)
	}
	return nil
}

// TODO: move to IC
var dependsRe = regexp.MustCompile(`(\.[\d]+)`)

func (ic *importContext) reference(i importable, path []string, value string, ctyValue cty.Value) hclwrite.Tokens {
	match := dependsRe.ReplaceAllString(strings.Join(path, "."), "")
	// TODO: get reference candidate, but if it's a `data`, then look for another non-data reference if possible..
	for _, d := range i.Depends {
		if d.Path != match {
			continue
		}
		if d.File {
			relativeFile := fmt.Sprintf("${path.module}/%s", value)
			return hclwrite.Tokens{
				&hclwrite.Token{Type: hclsyntax.TokenOQuote, Bytes: []byte{'"'}},
				&hclwrite.Token{Type: hclsyntax.TokenQuotedLit, Bytes: []byte(relativeFile)},
				&hclwrite.Token{Type: hclsyntax.TokenCQuote, Bytes: []byte{'"'}},
			}
		}
		if d.Variable {
			return ic.variable(fmt.Sprintf("%s_%s", path[0], value), "")
		}

		if tokens := ic.getTraversalTokens(d, value); tokens != nil {
			return tokens
		}
	}
	return hclwrite.TokensForValue(ctyValue)
}

func (ic *importContext) variable(name, desc string) hclwrite.Tokens {
	ic.variables[name] = desc
	return hclwrite.TokensForTraversal(hcl.Traversal{
		hcl.TraverseRoot{Name: "var"},
		hcl.TraverseAttr{Name: name},
	})
}

type fieldTuple struct {
	Field  string
	Schema *schema.Schema
}

func (ic *importContext) dataToHcl(i importable, path []string,
	pr *schema.Resource, d *schema.ResourceData, body *hclwrite.Body) error {
	ss := []fieldTuple{}
	for a, as := range pr.Schema {
		ss = append(ss, fieldTuple{a, as})
	}
	sort.Slice(ss, func(i, j int) bool {
		// it just happens that reverse field order
		// makes the most beautiful configs
		return ss[i].Field > ss[j].Field
	})
	for _, tuple := range ss {
		a, as := tuple.Field, tuple.Schema
		pathString := strings.Join(append(path, a), ".")
		raw, nonZero := d.GetOk(pathString)
		// log.Printf("[DEBUG] path=%s, raw='%v'", pathString, raw)
		if i.ShouldOmitField == nil { // we don't have custom function, so skip computed & default fields
			if defaultShouldOmitFieldFunc(ic, pathString, as, d) {
				continue
			}
		} else if i.ShouldOmitField(ic, pathString, as, d) {
			continue
		}
		mpath := dependsRe.ReplaceAllString(pathString, "")
		for _, r := range i.Depends {
			if r.Path == mpath && r.Variable {
				// sensitive fields are moved to variable depends, variable name is normalized
				// TODO: handle a case when we have multiple blocks, so names won't be unique
				raw = ic.regexFix(i.Name(ic, d), simpleNameFixes)
				nonZero = true
			}
		}
		shouldSkip := !nonZero
		if as.Required { // for required fields we must produce a value, even empty...
			shouldSkip = false
		} else if as.Default != nil && !reflect.DeepEqual(raw, as.Default) {
			// In case when have zero value, but there is non-zero default, we also need to produce it
			shouldSkip = false
		}
		if shouldSkip {
			continue
		}
		switch as.Type {
		case schema.TypeString:
			value := raw.(string)
			tokens := ic.reference(i, append(path, a), value, cty.StringVal(value))
			body.SetAttributeRaw(a, tokens)
		case schema.TypeBool:
			body.SetAttributeValue(a, cty.BoolVal(raw.(bool)))
		case schema.TypeInt:
			var num int64
			switch iv := raw.(type) {
			case int:
				num = int64(iv)
			case int32:
				num = int64(iv)
			case int64:
				num = iv
			}
			body.SetAttributeRaw(a, ic.reference(i, append(path, a),
				strconv.FormatInt(num, 10), cty.NumberIntVal(num)))
		case schema.TypeFloat:
			body.SetAttributeValue(a, cty.NumberFloatVal(raw.(float64)))
		case schema.TypeMap:
			// TODO: Resolve references in maps as well, and also support different types inside map...
			ov := map[string]cty.Value{}
			for key, iv := range raw.(map[string]any) {
				v := cty.StringVal(fmt.Sprintf("%v", iv))
				ov[key] = v
			}
			body.SetAttributeValue(a, cty.ObjectVal(ov))
		case schema.TypeSet:
			if rawSet, ok := raw.(*schema.Set); ok {
				rawList := rawSet.List()
				err := ic.readListFromData(i, append(path, a), d, rawList, body, as, func(i int) string {
					return strconv.Itoa(rawSet.F(rawList[i]))
				})
				if err != nil {
					return err
				}
			}
		case schema.TypeList:
			if rawList, ok := raw.([]any); ok {
				err := ic.readListFromData(i, append(path, a), d, rawList, body, as, strconv.Itoa)
				if err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported schema type: %v", path)
		}
	}
	return nil
}

func (ic *importContext) readListFromData(i importable, path []string, d *schema.ResourceData,
	rawList []any, body *hclwrite.Body, as *schema.Schema,
	offsetConverter func(i int) string) error {
	if len(rawList) == 0 {
		return nil
	}
	name := path[len(path)-1]
	switch elem := as.Elem.(type) {
	case *schema.Resource:
		if as.MaxItems == 1 {
			nestedPath := append(path, offsetConverter(0))
			confBlock := body.AppendNewBlock(name, []string{})
			return ic.dataToHcl(i, nestedPath, elem, d, confBlock.Body())
		}
		for offset := range rawList {
			confBlock := body.AppendNewBlock(name, []string{})
			nestedPath := append(path, offsetConverter(offset))
			err := ic.dataToHcl(i, nestedPath, elem, d, confBlock.Body())
			if err != nil {
				return err
			}
		}
	case *schema.Schema:
		toks := hclwrite.Tokens{}
		toks = append(toks, &hclwrite.Token{
			Type:  hclsyntax.TokenOBrack,
			Bytes: []byte{'['},
		})
		for _, raw := range rawList {
			if len(toks) != 1 {
				toks = append(toks, &hclwrite.Token{
					Type:  hclsyntax.TokenComma,
					Bytes: []byte{','},
				})
			}
			switch x := raw.(type) {
			case string:
				value := raw.(string)
				toks = append(toks, ic.reference(i, path, value, cty.StringVal(value))...)
			case int:
				// probably we don't even use integer lists?...
				toks = append(toks, hclwrite.TokensForValue(
					cty.NumberIntVal(int64(x)))...)
			default:
				return fmt.Errorf("unsupported primitive list: %#v", path)
			}
		}
		toks = append(toks, &hclwrite.Token{
			Type:  hclsyntax.TokenCBrack,
			Bytes: []byte{']'},
		})
		body.SetAttributeRaw(name, toks)
	}
	return nil
}
