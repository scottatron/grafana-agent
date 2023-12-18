package flow_test

// This file contains tests which verify that the Flow controller correctly updates and caches modules' arguments
// and exports in presence of multiple components.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/grafana/agent/pkg/flow"
	"github.com/grafana/agent/pkg/flow/internal/testcomponents"
	"github.com/stretchr/testify/require"

	_ "github.com/grafana/agent/component/module/string"
)

func TestImportModule(t *testing.T) {
	// We use this module in a Flow config below.
	module := `
	declare "test" {
		argument "input" {
			optional = false
		}
	
		testcomponents.passthrough "pt" {
			input = argument.input.value
			lag = "1ms"
		}
	
		export "output" {
			value = testcomponents.passthrough.pt.output
		}
	}
`
	filename := "my_module"
	require.NoError(t, os.WriteFile(filename, []byte(module), 0664))

	// We send the count increments via module and to the summation component and verify that the updates propagate.
	config := `
	testcomponents.count "inc" {
		frequency = "10ms"
		max = 10
	}

	import.file "testImport" {
		filename = "my_module"
	}

	testImport.test "myModule" {
		input = testcomponents.count.inc.count
	}

	testcomponents.summation "sum" {
		input = testImport.test.myModule.exports.output
	}
`

	ctrl := flow.New(testOptions(t))
	f, err := flow.ParseSource(t.Name(), []byte(config))
	require.NoError(t, err)
	require.NotNil(t, f)

	err = ctrl.LoadSource(f, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		export := getExport[testcomponents.SummationExports](t, ctrl, "", "testcomponents.summation.sum")
		return export.LastAdded == 10
	}, 3*time.Second, 10*time.Millisecond)

	// update the file to check if the module is correctly updated without reload
	newModule := `
	declare "test" {
		argument "input" {
			optional = false
		}

		testcomponents.passthrough "pt" {
			input = argument.input.value
			lag = "1ms"
		}

		export "output" {
			value = -10
		}
	}
	`
	require.NoError(t, os.WriteFile(filename, []byte(newModule), 0664))
	require.Eventually(t, func() bool {
		export := getExport[testcomponents.SummationExports](t, ctrl, "", "testcomponents.summation.sum")
		return export.LastAdded == -10
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(filename))
}

func TestImportModuleNoArgs(t *testing.T) {
	// We use this module in a Flow config below.
	module := `
	declare "test" {
		testcomponents.passthrough "pt" {
			input = 10
			lag = "1ms"
		}

		export "output" {
			value = testcomponents.passthrough.pt.output
		}
	}
`
	filename := "my_module"
	require.NoError(t, os.WriteFile(filename, []byte(module), 0664))

	config := `
import.file "testImport" {
	filename = "my_module"
}

testImport.test "myModule" {
}

testcomponents.summation "sum" {
	input = testImport.test.myModule.exports.output
}
`

	ctrl := flow.New(testOptions(t))
	f, err := flow.ParseSource(t.Name(), []byte(config))
	require.NoError(t, err)
	require.NotNil(t, f)

	err = ctrl.LoadSource(f, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		export := getExport[testcomponents.SummationExports](t, ctrl, "", "testcomponents.summation.sum")
		return export.LastAdded == 10
	}, 3*time.Second, 10*time.Millisecond)

	newModule := `
	declare "test" {
		testcomponents.passthrough "pt" {
			input = -10
			lag = "1ms"
		}
		
		export "output" {
			value = testcomponents.passthrough.pt.output
		}
	}
`
	require.NoError(t, os.WriteFile(filename, []byte(newModule), 0664))
	require.Eventually(t, func() bool {
		export := getExport[testcomponents.SummationExports](t, ctrl, "", "testcomponents.summation.sum")
		return export.LastAdded == -10
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(filename))
}

func TestNextImportModule(t *testing.T) {
	// We use this module in a Flow config below.
	module := `
import.file "otherModule" {
	filename = "other_module"
}
`
	otherModule := `
declare "test" {
	argument "input" {
		optional = false
	}

	testcomponents.passthrough "pt" {
		input = argument.input.value
		lag = "1ms"
	}

	export "output" {
		value = testcomponents.passthrough.pt.output
	}
}
`
	filename := "my_module"
	require.NoError(t, os.WriteFile(filename, []byte(module), 0664))
	otherFilename := "other_module"
	require.NoError(t, os.WriteFile(otherFilename, []byte(otherModule), 0664))

	// We send the count increments via module and to the summation component and verify that the updates propagate.
	config := `
	testcomponents.count "inc" {
		frequency = "10ms"
		max = 10
	}

	import.file "testImport" {
		filename = "my_module"
	}

	testImport.otherModule.test "myModule" {
		input = testcomponents.count.inc.count
	}

	testcomponents.summation "sum" {
		input = testImport.otherModule.test.myModule.exports.output
	}
`

	ctrl := flow.New(testOptions(t))
	f, err := flow.ParseSource(t.Name(), []byte(config))
	require.NoError(t, err)
	require.NotNil(t, f)

	err = ctrl.LoadSource(f, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		export := getExport[testcomponents.SummationExports](t, ctrl, "", "testcomponents.summation.sum")
		return export.LastAdded == 10
	}, 3*time.Second, 10*time.Millisecond)

	// update the file to check if the module is correctly updated without reload
	newOtherModule := `
	declare "test" {
		argument "input" {
			optional = false
		}

		testcomponents.passthrough "pt" {
			input = argument.input.value
			lag = "1ms"
		}

		export "output" {
			value = -10
		}
	}
	`
	require.NoError(t, os.WriteFile(otherFilename, []byte(newOtherModule), 0664))
	require.Eventually(t, func() bool {
		export := getExport[testcomponents.SummationExports](t, ctrl, "", "testcomponents.summation.sum")
		return export.LastAdded == -10
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(filename))
	require.NoError(t, os.Remove(otherFilename))
}
