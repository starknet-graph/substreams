package service

import (
	"github.com/streamingfast/dmetering"
	"github.com/streamingfast/substreams/pipeline"
	"github.com/streamingfast/substreams/wasm"
)

type anyTierService interface{}

type Option func(anyTierService)

func WithBytesMeter(bm dmetering.Meter) Option {
	if bm == nil { //guard against any weird nils
		bm = dmetering.NewBytesMeter()
	}

	return func(a anyTierService) {
		switch s := a.(type) {
		case *Tier1Service:
			s.bytesMeter = bm
		case *Tier2Service:
			s.bytesMeter = bm
		}
	}
}

func WithWASMExtension(ext wasm.WASMExtensioner) Option {
	return func(a anyTierService) {
		switch s := a.(type) {
		case *Tier1Service:
			s.wasmExtensions = append(s.wasmExtensions, ext)
		case *Tier2Service:
			s.wasmExtensions = append(s.wasmExtensions, ext)
		}
	}
}

// WithPipelineOptions is used to configure pipeline options for
// consumer outside of the substreams library itself, for example
// in chain specific Firehose implementations.
func WithPipelineOptions(f pipeline.PipelineOptioner) Option {
	return func(a anyTierService) {
		switch s := a.(type) {
		case *Tier1Service:
			s.pipelineOptions = append(s.pipelineOptions, f)
		case *Tier2Service:
			s.pipelineOptions = append(s.pipelineOptions, f)
		}
	}
}

func WithRequestStats() Option {
	return func(a anyTierService) {
		switch s := a.(type) {
		case *Tier1Service:
			s.runtimeConfig.WithRequestStats = true
		case *Tier2Service:
			s.runtimeConfig.WithRequestStats = true
		}
	}
}

func WithMaxWasmFuelPerBlockModule(maxFuel uint64) Option {
	return func(a anyTierService) {
		switch s := a.(type) {
		case *Tier1Service:
			s.runtimeConfig.MaxWasmFuel = maxFuel
		case *Tier2Service:
			s.runtimeConfig.MaxWasmFuel = maxFuel
		}
	}
}

func WithModuleExecutionTracing() Option {
	return func(a anyTierService) {
		switch s := a.(type) {
		case *Tier1Service:
			s.runtimeConfig.ModuleExecutionTracing = true
		case *Tier2Service:
			s.runtimeConfig.ModuleExecutionTracing = true
		}
	}
}
