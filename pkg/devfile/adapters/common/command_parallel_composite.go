package common

import (
	"fmt"

	"github.com/redhat-developer/odo/pkg/util"
)

// newParallelCompositeCommand creates a new command implementation which will execute the provided commands in parallel
func newParallelCompositeCommand(cmds ...command) command {
	return parallelCompositeCommand{
		cmds: cmds,
	}
}

// parallelCompositeCommand is a command implementation that represents parallel composite commands
type parallelCompositeCommand struct {
	cmds []command
}

func (p parallelCompositeCommand) Execute(show bool) error {
	// Loop over each command and execute it in parallel
	commandExecs := util.NewConcurrentTasks(len(p.cmds))
	for _, command := range p.cmds {
		cmd := command // needed to prevent the lambda from capturing the value
		commandExecs.Add(util.ConcurrentTask{ToRun: func(errChannel chan error) {
			err := cmd.Execute(show)
			if err != nil {
				errChannel <- err
			}
		}})
	}

	err := commandExecs.Run()
	if err != nil {
		return fmt.Errorf("parallel command execution failed: %w", err)
	}
	return nil
}

func (p parallelCompositeCommand) UnExecute() error {
	// Loop over each command and un-execute it in parallel
	commandExecs := util.NewConcurrentTasks(len(p.cmds))
	for _, command := range p.cmds {
		cmd := command // needed to prevent the lambda from capturing the value
		commandExecs.Add(util.ConcurrentTask{ToRun: func(errChannel chan error) {
			err := cmd.UnExecute()
			if err != nil {
				errChannel <- err
			}
		}})
	}

	err := commandExecs.Run()
	if err != nil {
		return fmt.Errorf("parallel command execution failed: %w", err)
	}
	return nil
}
