package pipeline

import (
	"context"
	"fmt"
	"io"
	"math"
	"runtime/debug"
	"strings"

	"github.com/streamingfast/bstream"
	"github.com/streamingfast/dstore"
	"github.com/streamingfast/substreams"
	"github.com/streamingfast/substreams/manifest"
	"github.com/streamingfast/substreams/orchestrator"
	"github.com/streamingfast/substreams/orchestrator/worker"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"github.com/streamingfast/substreams/pipeline/outputs"
	"github.com/streamingfast/substreams/state"
	"github.com/streamingfast/substreams/wasm"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Pipeline struct {
	vmType    string // wasm/rust-v1, native
	blockType string

	requestedStartBlockNum uint64 // rename to: requestStartBlock, SET UPON receipt of the request
	maxStoreSyncRangeSize  uint64
	isBackprocessing       bool

	preBlockHooks  []substreams.BlockHook
	postBlockHooks []substreams.BlockHook
	postJobHooks   []substreams.PostJobHook

	wasmRuntime    *wasm.Runtime
	wasmExtensions []wasm.WASMExtensioner
	storesMap      map[string]*state.Store
	stores         []*state.Store

	context context.Context
	request *pbsubstreams.Request
	graph   *manifest.ModuleGraph

	modules           []*pbsubstreams.Module
	outputModuleNames []string
	outputModuleMap   map[string]bool
	outputModules     []*pbsubstreams.Module
	leafStores        []*state.Store // in orchestrated execution
	moduleExecutors   []ModuleExecutor
	wasmOutputs       map[string][]byte

	baseStateStore     dstore.Store
	storesSaveInterval uint64

	clock         *pbsubstreams.Clock
	moduleOutputs []*pbsubstreams.ModuleOutput
	logs          []string

	moduleOutputCache *outputs.ModulesOutputCache

	currentBlockRef bstream.BlockRef

	outputCacheSaveBlockInterval uint64
	blockRangeSizeSubrequests    int
	grpcClientFactory            func() (pbsubstreams.StreamClient, []grpc.CallOption, error)
}

func New(
	ctx context.Context,
	request *pbsubstreams.Request,
	graph *manifest.ModuleGraph,
	blockType string,
	baseStateStore dstore.Store,
	outputCacheSaveBlockInterval uint64,
	wasmExtensions []wasm.WASMExtensioner,
	grpcClientFactory func() (pbsubstreams.StreamClient, []grpc.CallOption, error),
	blockRangeSizeSubRequests int,
	opts ...Option) *Pipeline {

	pipe := &Pipeline{
		context: ctx,
		request: request,
		// WARN: we don't support < 0 StartBlock for now
		requestedStartBlockNum:       uint64(request.StartBlockNum),
		storesMap:                    map[string]*state.Store{},
		graph:                        graph,
		baseStateStore:               baseStateStore,
		outputModuleNames:            request.OutputModules,
		outputModuleMap:              map[string]bool{},
		blockType:                    blockType,
		wasmExtensions:               wasmExtensions,
		grpcClientFactory:            grpcClientFactory,
		outputCacheSaveBlockInterval: outputCacheSaveBlockInterval,
		blockRangeSizeSubrequests:    blockRangeSizeSubRequests,

		maxStoreSyncRangeSize: math.MaxUint64,
	}

	for _, name := range request.OutputModules {
		pipe.outputModuleMap[name] = true
	}

	for _, opt := range opts {
		opt(pipe)
	}

	return pipe
}

func (p *Pipeline) HandlerFactory(workerPool *worker.Pool, respFunc func(resp *pbsubstreams.Response) error) (out bstream.Handler, err error) {
	ctx := p.context
	zlog.Info("initializing handler", zap.Uint64("requested_start_block", p.requestedStartBlockNum), zap.Uint64("requested_stop_block", p.request.StopBlockNum), zap.Bool("is_orchestrated_execution", p.isBackprocessing), zap.Strings("outputs", p.request.OutputModules))

	p.moduleOutputCache = outputs.NewModuleOutputCache(p.outputCacheSaveBlockInterval)

	if err := p.build(); err != nil {
		return nil, fmt.Errorf("building pipeline: %w", err)
	}

	stores := p.stores

	for _, module := range p.modules {
		isOutput := p.outputModuleMap[module.Name]
		p.outputModules = append(p.outputModules, module)

		if isOutput && p.requestedStartBlockNum < module.InitialBlock {
			return nil, fmt.Errorf("invalid request: start block %d smaller that request outputs for module: %q start block %d", p.requestedStartBlockNum, module.Name, module.InitialBlock)
		}

		hash := manifest.HashModuleAsString(p.request.Modules, p.graph, module)
		_, err := p.moduleOutputCache.RegisterModule(ctx, module, hash, p.baseStateStore, p.requestedStartBlockNum)
		if err != nil {
			return nil, fmt.Errorf("registering output cache for module %q: %w", module.Name, err)
		}
	}

	if p.isBackprocessing {
		totalOutputModules := len(p.outputModuleNames)
		outputName := p.outputModuleNames[0]
		buildingStore := p.storesMap[outputName]
		isLastStore := len(stores) > 0 && stores[len(stores)-1] == buildingStore

		if totalOutputModules == 1 && buildingStore != nil && isLastStore {
			// totalOutputModels is a temporary restrictions, for when the orchestrator
			// will be able to run two leaf stores from the same job
			zlog.Info("marking leaf store for partial processing", zap.String("module", outputName))
			buildingStore.StoreInitialBlock = p.requestedStartBlockNum
			p.leafStores = append(p.leafStores, buildingStore)
		} else {
			zlog.Info("conditions for leaf store not met",
				zap.String("module", outputName),
				zap.Bool("is_last_store", isLastStore),
				zap.Int("output_module_count", totalOutputModules))
		}

		zlog.Info("initializing and loading stores")
		if err = p.LoadStores(ctx); err != nil {
			return nil, fmt.Errorf("loading stores: %w", err)
		}
	} else {
		// This launches processing for all depend stores at the requests' `startBlock`
		err = SynchronizeStores(
			ctx,
			workerPool,
			p.request,
			stores,
			p.graph, p.moduleOutputCache.OutputCaches, p.requestedStartBlockNum, respFunc, p.blockRangeSizeSubrequests,
			p.storesSaveInterval,
			p.maxStoreSyncRangeSize,
		)
		if err != nil {
			return nil, fmt.Errorf("synchronizing stores: %w", err)
		}
		// All STORES are expected to be synchronized properly at this point, and have
		// data up until requestedStartBlockNum.
	}

	err = p.buildWASM(ctx, p.request, p.modules)
	if err != nil {
		return nil, fmt.Errorf("initiating module output caches: %w", err)
	}

	for _, cache := range p.moduleOutputCache.OutputCaches {
		atBlock := outputs.ComputeStartBlock(p.requestedStartBlockNum, p.outputCacheSaveBlockInterval)
		if _, err := cache.Load(ctx, atBlock); err != nil {
			return nil, fmt.Errorf("loading outputs caches")
		}
	}

	return bstream.HandlerFunc(func(block *bstream.Block, obj interface{}) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic at block %d: %s", block.Num(), r)
				zlog.Error("panic while process block", zap.Uint64("block_num", block.Num()), zap.Error(err))
				zlog.Error(string(debug.Stack()))
			}
			if err != nil {
				for _, hook := range p.postJobHooks {
					if err := hook(ctx, p.clock); err != nil {
						zlog.Warn("post job hook failed", zap.Error(err))
					}
				}
			}
		}()

		// TODO(abourget): eventually, handle the `undo` signals.

		p.clock = &pbsubstreams.Clock{
			Number:    block.Num(),
			Id:        block.Id,
			Timestamp: timestamppb.New(block.Time()),
		}

		p.currentBlockRef = block.AsRef()

		if err = p.moduleOutputCache.Update(ctx, p.currentBlockRef); err != nil {
			return fmt.Errorf("updating module output cache: %w", err)
		}

		for _, hook := range p.preBlockHooks {
			if err := hook(ctx, p.clock); err != nil {
				return fmt.Errorf("pre block hook: %w", err)
			}
		}

		p.moduleOutputs = nil
		p.wasmOutputs = map[string][]byte{}

		//todo? should we only save store if in partial mode or in catchup?
		// no need to save store if loaded from cache?
		isFirstRequestBlock := p.requestedStartBlockNum == p.clock.Number
		intervalReached := p.storesSaveInterval != 0 && p.clock.Number%p.storesSaveInterval == 0
		isTemporaryStore := p.isBackprocessing && p.clock.Number == p.request.StopBlockNum && p.request.StopBlockNum != 0

		if !isFirstRequestBlock && (intervalReached || isTemporaryStore) {
			if err := p.saveStoresSnapshots(ctx, p.clock.Number); err != nil {
				return fmt.Errorf("saving stores: %w", err)
			}
		}

		if p.clock.Number >= p.request.StopBlockNum && p.request.StopBlockNum != 0 {
			// FIXME: HERE WE KNOW THAT we've gone OVER the ExclusiveEnd boundary,
			// and we will trigger this EVEN if we have chains that SKIP BLOCKS.

			if p.isBackprocessing {
				// TODO: why wouldn't we do that when we're live?! Why only when orchestrated?
				zlog.Debug("about to save cache output", zap.Uint64("clock", p.clock.Number), zap.Uint64("stop_block", p.request.StopBlockNum))
				if err := p.moduleOutputCache.Save(ctx); err != nil {
					return fmt.Errorf("saving partial caches")
				}
			}
			return io.EOF
		}

		zlog.Debug("processing block", zap.Uint64("block_num", block.Number))

		cursor := obj.(bstream.Cursorable).Cursor()
		step := obj.(bstream.Stepable).Step()

		if err = p.assignSource(block); err != nil {
			return fmt.Errorf("setting up sources: %w", err)
		}

		for _, executor := range p.moduleExecutors {
			zlog.Debug("executing", zap.Stringer("module_name", executor))
			err := executor.run(p.wasmOutputs, p.clock, block)
			if err != nil {
				if returnErr := p.returnFailureProgress(err, executor, respFunc); returnErr != nil {
					return returnErr
				}

				return err
			}

			logs, truncated := executor.moduleLogs()

			p.moduleOutputs = append(p.moduleOutputs, &pbsubstreams.ModuleOutput{
				Name:          executor.Name(),
				Data:          executor.moduleOutputData(),
				Logs:          logs,
				LogsTruncated: truncated,
			})
		}

		if p.clock.Number >= p.requestedStartBlockNum {
			if err := p.returnOutputs(step, cursor, respFunc); err != nil {
				return err
			}
		}

		for _, s := range p.storesMap {
			s.Flush()
		}

		zlog.Debug("block processed", zap.Uint64("block_num", block.Number))
		return nil
	}), nil
}

func (p *Pipeline) returnOutputs(step bstream.StepType, cursor *bstream.Cursor, respFunc substreams.ResponseFunc) error {
	if len(p.moduleOutputs) > 0 {
		zlog.Debug("got modules outputs", zap.Int("module_output_count", len(p.moduleOutputs)))
		out := &pbsubstreams.BlockScopedData{
			Outputs: p.moduleOutputs,
			Clock:   p.clock,
			Step:    pbsubstreams.StepToProto(step),
			Cursor:  cursor.ToOpaque(),
		}

		if err := respFunc(substreams.NewBlockScopedDataResponse(out)); err != nil {
			return fmt.Errorf("calling return func: %w", err)
		}
	}

	if p.isBackprocessing {
		// TODO(abourget): we might want to send progress for the segment after batch execution
		var progress []*pbsubstreams.ModuleProgress

		for _, store := range p.leafStores {
			progress = append(progress, &pbsubstreams.ModuleProgress{
				Name: store.Name,
				Type: &pbsubstreams.ModuleProgress_ProcessedRanges{
					ProcessedRanges: &pbsubstreams.ModuleProgress_ProcessedRange{
						ProcessedRanges: []*pbsubstreams.BlockRange{
							{
								StartBlock: store.StoreInitialBlock,
								EndBlock:   p.clock.Number,
							},
						},
					},
				},
			})
		}

		if err := respFunc(substreams.NewModulesProgressResponse(progress)); err != nil {
			return fmt.Errorf("calling return func: %w", err)
		}
	}
	return nil
}

func (p *Pipeline) returnFailureProgress(err error, failedExecutor ModuleExecutor, respFunc substreams.ResponseFunc) error {
	modules := make([]*pbsubstreams.ModuleProgress, len(p.moduleOutputs)+1)

	for i, moduleOutput := range p.moduleOutputs {
		modules[i] = &pbsubstreams.ModuleProgress{
			Name: moduleOutput.Name,
			Type: &pbsubstreams.ModuleProgress_Failed_{
				Failed: &pbsubstreams.ModuleProgress_Failed{
					Logs:          moduleOutput.Logs,
					LogsTruncated: moduleOutput.LogsTruncated,
				},
			},
		}
	}

	logs, truncated := failedExecutor.moduleLogs()

	modules[len(p.moduleOutputs)] = &pbsubstreams.ModuleProgress{
		Name: failedExecutor.Name(),

		Type: &pbsubstreams.ModuleProgress_Failed_{
			Failed: &pbsubstreams.ModuleProgress_Failed{
				// Should we maybe extract specific WASM error and improved the "printing" here?
				Reason:        err.Error(),
				Logs:          logs,
				LogsTruncated: truncated,
			},
		},
	}

	return respFunc(substreams.NewModulesProgressResponse(modules))
}

func (p *Pipeline) assignSource(block *bstream.Block) error {
	switch p.vmType {
	case "wasm/rust-v1":
		blkBytes, err := block.Payload.Get()
		if err != nil {
			return fmt.Errorf("getting block %d %q: %w", block.Number, block.Id, err)
		}

		clockBytes, err := proto.Marshal(p.clock)

		p.wasmOutputs[p.blockType] = blkBytes
		p.wasmOutputs["sf.substreams.v1.Clock"] = clockBytes
	default:
		panic("unsupported vmType " + p.vmType)
	}
	return nil
}

func (p *Pipeline) build() error {
	if err := p.validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if err := p.buildModules(); err != nil {
		return fmt.Errorf("build modules graph: %w", err)
	}
	if err := p.buildStores(); err != nil {
		return fmt.Errorf("build stores graph: %w", err)
	}
	return nil
}

func (p *Pipeline) validate() error {
	for _, binary := range p.request.Modules.Binaries {
		if binary.Type != "wasm/rust-v1" {
			return fmt.Errorf("unsupported binary type: %q, supported: %q", binary.Type, p.vmType)
		}
		p.vmType = binary.Type
	}
	return nil
}

func (p *Pipeline) buildModules() error {
	modules, err := p.graph.ModulesDownTo(p.outputModuleNames)
	if err != nil {
		return fmt.Errorf("building execution graph: %w", err)
	}
	p.modules = modules
	return nil
}

func (p *Pipeline) buildStores() error {
	storeModules, err := p.graph.StoresDownTo(p.outputModuleNames)
	if err != nil {
		return err
	}

	p.storesMap = make(map[string]*state.Store)
	p.stores = nil
	for _, storeModule := range storeModules {
		var options []state.BuilderOption

		builder, err := state.NewBuilder(
			storeModule.Name,
			p.storesSaveInterval,
			storeModule.InitialBlock,
			manifest.HashModuleAsString(p.request.Modules, p.graph, storeModule),
			storeModule.GetKindStore().UpdatePolicy,
			storeModule.GetKindStore().ValueType,
			p.baseStateStore,
			options...,
		)
		if err != nil {
			return fmt.Errorf("creating builder %s: %w", storeModule.Name, err)
		}

		p.stores = append(p.stores, builder)

		p.storesMap[builder.Name] = builder
	}

	return nil
}

func (p *Pipeline) buildWASM(ctx context.Context, request *pbsubstreams.Request, modules []*pbsubstreams.Module) error {
	p.wasmOutputs = map[string][]byte{}
	p.wasmRuntime = wasm.NewRuntime(p.wasmExtensions)

	for _, module := range modules {
		isOutput := p.outputModuleMap[module.Name]
		var inputs []*wasm.Input

		for _, input := range module.Inputs {
			switch in := input.Input.(type) {
			case *pbsubstreams.Module_Input_Map_:
				inputs = append(inputs, &wasm.Input{
					Type: wasm.InputSource,
					Name: in.Map.ModuleName,
				})
			case *pbsubstreams.Module_Input_Store_:
				inputName := input.GetStore().ModuleName
				if input.GetStore().Mode == pbsubstreams.Module_Input_Store_DELTAS {
					inputs = append(inputs, &wasm.Input{
						Type:   wasm.InputStore,
						Name:   inputName,
						Store:  p.storesMap[inputName],
						Deltas: true,
					})
				} else {
					inputs = append(inputs, &wasm.Input{
						Type:  wasm.InputStore,
						Name:  inputName,
						Store: p.storesMap[inputName],
					})
					if p.storesMap[inputName] == nil {
						return fmt.Errorf("no store with name %q", inputName)
					}
				}

			case *pbsubstreams.Module_Input_Source_:
				inputs = append(inputs, &wasm.Input{
					Type: wasm.InputSource,
					Name: in.Source.Type,
				})
			default:
				return fmt.Errorf("invalid input struct for module %q", module.Name)
			}
		}

		modName := module.Name // to ensure it's enclosed
		entrypoint := module.BinaryEntrypoint
		code := p.request.Modules.Binaries[module.BinaryIndex]
		wasmModule, err := p.wasmRuntime.NewModule(ctx, request, code.Content, module.Name)
		if err != nil {
			return fmt.Errorf("new wasm module: %w", err)
		}

		switch kind := module.Kind.(type) {
		case *pbsubstreams.Module_KindMap_:
			outType := strings.TrimPrefix(module.Output.Type, "proto:")

			executor := &MapperModuleExecutor{
				BaseExecutor: BaseExecutor{
					moduleName: module.Name,
					wasmModule: wasmModule,
					entrypoint: entrypoint,
					wasmInputs: inputs,
					isOutput:   isOutput,
					cache:      p.moduleOutputCache.OutputCaches[module.Name],
				},
				outputType: outType,
			}

			p.moduleExecutors = append(p.moduleExecutors, executor)
			continue
		case *pbsubstreams.Module_KindStore_:
			updatePolicy := kind.KindStore.UpdatePolicy
			valueType := kind.KindStore.ValueType

			outputStore := p.storesMap[modName]
			inputs = append(inputs, &wasm.Input{
				Type:         wasm.OutputStore,
				Name:         modName,
				Store:        outputStore,
				UpdatePolicy: updatePolicy,
				ValueType:    valueType,
			})

			s := &StoreModuleExecutor{
				BaseExecutor: BaseExecutor{
					moduleName: modName,
					isOutput:   isOutput,
					wasmModule: wasmModule,
					entrypoint: entrypoint,
					wasmInputs: inputs,
					cache:      p.moduleOutputCache.OutputCaches[module.Name],
				},
				outputStore: outputStore,
			}

			p.moduleExecutors = append(p.moduleExecutors, s)
			continue
		default:
			return fmt.Errorf("invalid kind %q input module %q", module.Kind, module.Name)
		}
	}

	return nil
}

func SynchronizeStores(
	ctx context.Context,
	workerPool *worker.Pool,
	originalRequest *pbsubstreams.Request,
	builders []*state.Store,
	graph *manifest.ModuleGraph,
	outputCache map[string]*outputs.OutputCache,
	upToBlockNum uint64,
	respFunc substreams.ResponseFunc,
	blockRangeSizeSubRequests int,
	storeSaveInterval uint64,
	maxSubrequestRangeSize uint64) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	zlog.Info("synchronizing stores")

	requestPool := orchestrator.NewRequestPool()

	storageState, err := orchestrator.FetchStorageState(ctx, builders)
	if err != nil {
		return fmt.Errorf("fetching stores states: %w", err)
	}

	progressMessages := storageState.ProgressMessages()
	if err := respFunc(substreams.NewModulesProgressResponse(progressMessages)); err != nil {
		return fmt.Errorf("sending progress: %w", err)
	}

	squasher, err := orchestrator.NewSquasher(ctx, storageState, builders, outputCache, storeSaveInterval, upToBlockNum, orchestrator.WithNotifier(requestPool))
	if err != nil {
		return fmt.Errorf("initializing squasher: %w", err)
	}

	strategy, err := orchestrator.NewOrderedStrategy(ctx, storageState, originalRequest, builders, graph, requestPool, upToBlockNum, blockRangeSizeSubRequests, maxSubrequestRangeSize)
	if err != nil {
		return fmt.Errorf("creating strategy: %w", err)
	}

	scheduler, err := orchestrator.NewScheduler(ctx, strategy, squasher, workerPool, respFunc, blockRangeSizeSubRequests)
	if err != nil {
		return fmt.Errorf("initializing scheduler: %w", err)
	}

	requestCount := strategy.RequestCount()
	if requestCount == 0 {
		return nil
	}
	result := make(chan error)

	scheduler.Launch(ctx, result)

	resultCount := 0
done:
	for {
		select {
		case <-ctx.Done():
			return nil // FIXME: If we exit here without killing the go func() above, this will clog the `result` chan
		case err := <-result:
			resultCount++
			if err != nil {
				return fmt.Errorf("from worker: %w", err)
			}
			zlog.Debug("received result", zap.Int("result_count", resultCount), zap.Int("request_count", requestCount), zap.Error(err))
			if resultCount == requestCount {
				break done
			}
		}
	}

	zlog.Info("store sync completed")

	if err := squasher.StoresReady(); err != nil {
		return fmt.Errorf("squasher ready: %w", err)
	}

	return nil
}

func (p *Pipeline) saveStoresSnapshots(ctx context.Context, lastBlock uint64) error {
	// FIXME: lastBlock NEEDS to BE ALIGNED on boundaries!!
	for _, builder := range p.storesMap {
		// TODO: implement parallel writing and upload for the different stores involved.
		err := builder.WriteState(ctx, lastBlock)
		if err != nil {
			return fmt.Errorf("writing store '%s' state: %w", builder.Name, err)
		}

		if builder.IsPartial() {
			builder.Truncate()
			builder.Roll(lastBlock)
		}

		zlog.Info("state written", zap.String("store_name", builder.Name))
	}

	return nil
}

func (p *Pipeline) LoadStores(ctx context.Context) error {
	for _, builder := range p.storesMap {
		if builder.IsPartial() {
			continue
		}
		if builder.StoreInitialBlock == p.requestedStartBlockNum {
			continue
		}

		err := builder.Fetch(ctx, p.requestedStartBlockNum)
		if err != nil {
			return fmt.Errorf("reading state for builder %q: %w", builder.Name, err)
		}
	}
	return nil
}
