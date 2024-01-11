package controller

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/agent/component"
	importsource "github.com/grafana/agent/pkg/flow/internal/import-source"
	"github.com/grafana/agent/pkg/flow/logging/level"
	"github.com/grafana/agent/pkg/flow/tracing"
	"github.com/grafana/river/ast"
	"github.com/grafana/river/parser"
	"github.com/grafana/river/vm"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"
)

type ImportConfigNode struct {
	id                        ComponentID
	label                     string
	nodeID                    string
	componentName             string
	globalID                  string
	globals                   ComponentGlobals // Need a copy of the globals to create other import nodes.
	source                    importsource.ImportSource
	registry                  *prometheus.Registry
	importedContent           map[string]string
	importConfigNodesChildren map[string]*ImportConfigNode
	OnComponentUpdate         func(cn NodeWithDependants) // Informs controller that we need to reevaluate
	logger                    log.Logger
	inContentUpdate           bool

	mut                sync.RWMutex
	importedContentMut sync.RWMutex
	block              *ast.BlockStmt // Current River blocks to derive config from
	lastUpdateTime     atomic.Time

	healthMut  sync.RWMutex
	evalHealth component.Health // Health of the last evaluate
	runHealth  component.Health // Health of running the component
}

var _ NodeWithDependants = (*ImportConfigNode)(nil)
var _ RunnableNode = (*ImportConfigNode)(nil)
var _ ComponentNode = (*ImportConfigNode)(nil)
var _ ModuleContentProvider = (*ImportConfigNode)(nil)

// NewImportConfigNode creates a new ImportConfigNode from an initial ast.BlockStmt.
// The underlying config isn't applied until Evaluate is called.
func NewImportConfigNode(block *ast.BlockStmt, globals ComponentGlobals, sourceType importsource.SourceType) *ImportConfigNode {
	var (
		id     = BlockComponentID(block)
		nodeID = id.String()
	)

	initHealth := component.Health{
		Health:     component.HealthTypeUnknown,
		Message:    "component created",
		UpdateTime: time.Now(),
	}
	globalID := nodeID
	if globals.ControllerID != "" {
		globalID = path.Join(globals.ControllerID, nodeID)
	}
	cn := &ImportConfigNode{
		id:                id,
		globalID:          globalID,
		label:             block.Label,
		globals:           globals,
		nodeID:            BlockComponentID(block).String(),
		componentName:     block.GetBlockName(),
		importedContent:   make(map[string]string),
		OnComponentUpdate: globals.OnComponentUpdate,
		block:             block,
		evalHealth:        initHealth,
		runHealth:         initHealth,
	}
	managedOpts := getImportManagedOptions(globals, cn)
	cn.logger = managedOpts.Logger
	cn.source = importsource.NewImportSource(sourceType, managedOpts, vm.New(block.Body), cn.onContentUpdate)
	return cn
}

func getImportManagedOptions(globals ComponentGlobals, cn *ImportConfigNode) component.Options {
	cn.registry = prometheus.NewRegistry()
	return component.Options{
		ID:     cn.globalID,
		Logger: log.With(globals.Logger, "component", cn.globalID),
		Registerer: prometheus.WrapRegistererWith(prometheus.Labels{
			"component_id": cn.globalID,
		}, cn.registry),
		Tracer: tracing.WrapTracer(globals.TraceProvider, cn.globalID),

		DataPath: filepath.Join(globals.DataPath, cn.globalID),

		GetServiceData: func(name string) (interface{}, error) {
			return globals.GetServiceData(name)
		},
	}
}

// Evaluate implements BlockNode and updates the arguments for the managed config block
// by re-evaluating its River block with the provided scope. The managed config block
// will be built the first time Evaluate is called.
//
// Evaluate will return an error if the River block cannot be evaluated or if
// decoding to arguments fails.
func (cn *ImportConfigNode) Evaluate(scope *vm.Scope) error {
	err := cn.evaluate(scope)

	switch err {
	case nil:
		cn.setEvalHealth(component.HealthTypeHealthy, "component evaluated")
	default:
		msg := fmt.Sprintf("component evaluation failed: %s", err)
		cn.setEvalHealth(component.HealthTypeUnhealthy, msg)
	}
	return err
}

func (cn *ImportConfigNode) setEvalHealth(t component.HealthType, msg string) {
	cn.healthMut.Lock()
	defer cn.healthMut.Unlock()

	cn.evalHealth = component.Health{
		Health:     t,
		Message:    msg,
		UpdateTime: time.Now(),
	}
}

func (cn *ImportConfigNode) evaluate(scope *vm.Scope) error {
	cn.mut.Lock()
	defer cn.mut.Unlock()
	return cn.source.Evaluate(scope)
}

// processNodeBody processes the body of a node.
func (cn *ImportConfigNode) processNodeBody(node *ast.File, content string) {
	for _, stmt := range node.Body {
		switch stmt := stmt.(type) {
		case *ast.BlockStmt:
			fullName := strings.Join(stmt.Name, ".")
			switch fullName {
			case "declare":
				cn.processDeclareBlock(stmt, content)
			case importsource.BlockImportFile, importsource.BlockImportGit, importsource.BlockImportHTTP:
				cn.processImportBlock(stmt, fullName)
			default:
				level.Error(cn.logger).Log("msg", "only declare and import blocks are allowed in a module", "forbidden", fullName)
			}
		default:
			level.Error(cn.logger).Log("msg", "only declare and import blocks are allowed in a module")
		}
	}
}

// processDeclareBlock processes a declare block.
func (cn *ImportConfigNode) processDeclareBlock(stmt *ast.BlockStmt, content string) {
	if _, ok := cn.importedContent[stmt.Label]; ok {
		level.Error(cn.logger).Log("msg", "declare block redefined", "name", stmt.Label)
		return
	}
	cn.importedContent[stmt.Label] = content[stmt.LCurlyPos.Position().Offset+1 : stmt.RCurlyPos.Position().Offset-1]
}

// processDeclareBlock processes an import block.
func (cn *ImportConfigNode) processImportBlock(stmt *ast.BlockStmt, fullName string) {
	sourceType := importsource.GetSourceType(fullName)
	if _, ok := cn.importConfigNodesChildren[stmt.Label]; ok {
		level.Error(cn.logger).Log("msg", "import block redefined", "name", stmt.Label)
		return
	}
	childGlobals := cn.globals
	childGlobals.OnComponentUpdate = cn.OnChildrenContentUpdate
	cn.importConfigNodesChildren[stmt.Label] = NewImportConfigNode(stmt, childGlobals, sourceType)
}

// onContentUpdate is triggered every time the managed import component has new content.
func (cn *ImportConfigNode) onContentUpdate(content string) {
	cn.importedContentMut.Lock()
	defer cn.importedContentMut.Unlock()
	cn.inContentUpdate = true
	cn.importedContent = make(map[string]string)
	// TODO: We recreate the nodes when the content changes. Can we copy instead for optimization?
	cn.importConfigNodesChildren = make(map[string]*ImportConfigNode)
	node, err := parser.ParseFile(cn.label, []byte(content))
	if err != nil {
		level.Error(cn.logger).Log("msg", "failed to parse file on update", "err", err)
		return
	}
	cn.processNodeBody(node, content)
	cn.evaluateChildren()
	cn.lastUpdateTime.Store(time.Now())
	cn.OnComponentUpdate(cn)
	cn.inContentUpdate = false
}

// evaluateChildren evaluates the import nodes managed by this import node.
func (cn *ImportConfigNode) evaluateChildren() {
	for _, child := range cn.importConfigNodesChildren {
		child.Evaluate(&vm.Scope{
			Parent:    nil,
			Variables: make(map[string]interface{}),
		})
	}
}

// runChildren run the import nodes managed by this import node.
func (cn *ImportConfigNode) runChildren(ctx context.Context) error {
	var wg sync.WaitGroup
	errChildrenChan := make(chan error, len(cn.importConfigNodesChildren))

	for _, child := range cn.importConfigNodesChildren {
		wg.Add(1)
		go func(child *ImportConfigNode) {
			defer wg.Done()
			if err := child.Run(ctx); err != nil {
				errChildrenChan <- err
			}
		}(child)
	}

	go func() {
		wg.Wait()
		close(errChildrenChan)
	}()

	return <-errChildrenChan
}

// OnChildrenContentUpdate passes their imported content to their parents.
// To avoid collisions, the content is scoped via namespaces.
func (cn *ImportConfigNode) OnChildrenContentUpdate(child NodeWithDependants) {
	switch child := child.(type) {
	case *ImportConfigNode:
		for importedDeclareLabel, content := range child.importedContent {
			label := child.label + "." + importedDeclareLabel
			cn.importedContent[label] = content
		}
	}
	// This avoids to OnComponentUpdate to be called multiple times in a row when the content changes.
	if !cn.inContentUpdate {
		cn.OnComponentUpdate(cn)
	}
}

func (cn *ImportConfigNode) ModuleContent(scopedName string) (string, error) {
	cn.importedContentMut.Lock()
	defer cn.importedContentMut.Unlock()
	if content, ok := cn.importedContent[scopedName]; ok {
		return content, nil
	}
	return "", fmt.Errorf("module %s not found in imported node %s", scopedName, cn.label)
}

// Run runs the managed component in the calling goroutine until ctx is
// canceled. Evaluate must have been called at least once without returning an
// error before calling Run.
//
// Run will immediately return ErrUnevaluated if Evaluate has never been called
// successfully. Otherwise, Run will return nil.
func (cn *ImportConfigNode) Run(ctx context.Context) error {
	cn.mut.RLock()
	managed := cn.source
	cn.mut.RUnlock()

	if managed == nil {
		return ErrUnevaluated
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errChan := make(chan error, 1)

	if len(cn.importConfigNodesChildren) > 0 {
		go func() {
			errChan <- cn.runChildren(ctx)
		}()
	}

	cn.setRunHealth(component.HealthTypeHealthy, "started component")

	go func() {
		errChan <- managed.Run(ctx)
	}()

	err := <-errChan

	var exitMsg string
	if err != nil {
		level.Error(cn.logger).Log("msg", "component exited with error", "err", err)
		exitMsg = fmt.Sprintf("component shut down with error: %s", err)
	} else {
		level.Info(cn.logger).Log("msg", "component exited")
		exitMsg = "component shut down normally"
	}

	cn.setRunHealth(component.HealthTypeExited, exitMsg)
	return err
}

func (cn *ImportConfigNode) setRunHealth(t component.HealthType, msg string) {
	cn.healthMut.Lock()
	defer cn.healthMut.Unlock()

	cn.runHealth = component.Health{
		Health:     t,
		Message:    msg,
		UpdateTime: time.Now(),
	}
}

func (cn *ImportConfigNode) Label() string { return cn.label }

// Block implements BlockNode and returns the current block of the managed config node.
func (cn *ImportConfigNode) Block() *ast.BlockStmt {
	cn.mut.RLock()
	defer cn.mut.RUnlock()
	return cn.block
}

// NodeID implements dag.Node and returns the unique ID for the config node.
func (cn *ImportConfigNode) NodeID() string { return cn.nodeID }

// This node has no exports.
func (cn *ImportConfigNode) Exports() component.Exports {
	return nil
}

func (cn *ImportConfigNode) ID() ComponentID { return cn.id }

func (cn *ImportConfigNode) LastUpdateTime() time.Time {
	return cn.lastUpdateTime.Load()
}

// Arguments returns the current arguments of the managed component.
func (cn *ImportConfigNode) Arguments() component.Arguments {
	cn.mut.RLock()
	defer cn.mut.RUnlock()
	return cn.source.Arguments()
}

// Component returns the instance of the managed component. Component may be
// nil if the ComponentNode has not been successfully evaluated yet.
func (cn *ImportConfigNode) Component() component.Component {
	cn.mut.RLock()
	defer cn.mut.RUnlock()
	return cn.source.Component()
}

// CurrentHealth returns the current health of the ComponentNode.
//
// The health of a ComponentNode is determined by combining:
//
//  1. Health from the call to Run().
//  2. Health from the last call to Evaluate().
//  3. Health reported from the component.
func (cn *ImportConfigNode) CurrentHealth() component.Health {
	cn.healthMut.RLock()
	defer cn.healthMut.RUnlock()
	return component.LeastHealthy(cn.runHealth, cn.evalHealth, cn.source.CurrentHealth())
}

// FileComponent does not have DebugInfo
func (cn *ImportConfigNode) DebugInfo() interface{} {
	return nil
}

// This component does not manage modules.
func (cn *ImportConfigNode) ModuleIDs() []string {
	return nil
}

// BlockName returns the name of the block.
func (cn *ImportConfigNode) BlockName() string {
	return cn.componentName
}
