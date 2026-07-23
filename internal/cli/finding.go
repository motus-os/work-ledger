package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/motus-os/work-ledger/internal/ids"
	"github.com/motus-os/work-ledger/internal/store"
)

func runFinding(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	if len(arguments) == 0 || arguments[0] == "-h" || arguments[0] == "--help" {
		writeFindingHelp(environment.Stdout)
		return 0
	}
	switch arguments[0] {
	case "add":
		return runFindingAdd(ctx, arguments[1:], stateDir, environment)
	case "list":
		return runFindingList(ctx, arguments[1:], stateDir, environment)
	case "show":
		return runFindingShow(ctx, arguments[1:], stateDir, environment)
	case "close":
		return runFindingClose(ctx, arguments[1:], stateDir, environment)
	default:
		return usageError(environment.Stderr, fmt.Errorf("unknown finding command %q", arguments[0]))
	}
}

func runFindingAdd(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	var runID, file, format string
	jsonOutput := false
	for len(arguments) > 0 {
		argument := arguments[0]
		switch argument {
		case "--json":
			jsonOutput = true
			arguments = arguments[1:]
		case "--run", "--file", "--format":
			if len(arguments) < 2 || arguments[1] == "" {
				return usageError(environment.Stderr, fmt.Errorf("%s requires a value", argument))
			}
			value := arguments[1]
			arguments = arguments[2:]
			var target *string
			switch argument {
			case "--run":
				target = &runID
			case "--file":
				target = &file
			case "--format":
				target = &format
			}
			if *target != "" {
				return usageError(environment.Stderr, fmt.Errorf("%s may be specified only once", argument))
			}
			*target = value
		case "-h", "--help":
			writeFindingHelp(environment.Stdout)
			return 0
		default:
			return usageError(environment.Stderr, fmt.Errorf("unknown finding add option %q", argument))
		}
	}
	if runID == "" || file == "" {
		return usageError(environment.Stderr, errors.New("finding add requires --run and --file"))
	}
	if format == "" {
		format = "text"
	}
	content, err := readFindingContent(environment.Stdin, file, format)
	if errors.Is(err, store.ErrInvalid) {
		return usageError(environment.Stderr, err)
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	findingID, err := ids.New("finding_")
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	ledger, err := store.Open(ctx, stateDir)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	finding, err := ledger.AddFinding(ctx, store.AddFindingParams{
		ID: findingID, OriginRunID: runID, Content: content,
	})
	if errors.Is(err, store.ErrInvalid) {
		return usageError(environment.Stderr, err)
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	if jsonOutput {
		return writeJSON(environment.Stdout, finding, environment.Stderr)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Recorded %s (%s)\n", safeHumanText(finding.ID), finding.State)
	fmt.Fprintf(&output, "Run: %s\n", safeHumanText(finding.OriginRunID))
	fmt.Fprintf(&output, "Summary: %s\n", safeHumanText(finding.Content.Summary))
	fmt.Fprintf(&output, "%s %s\n", nextCommandLabel(), displayCommand(
		environment.ProgramName, "--state-dir", stateDir, "finding", "show", finding.ID,
	))
	return writeText(environment.Stdout, output.String(), environment.Stderr)
}

func runFindingClose(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	if len(arguments) == 0 || arguments[0] == "-h" || arguments[0] == "--help" {
		writeFindingHelp(environment.Stdout)
		return 0
	}
	findingID := arguments[0]
	arguments = arguments[1:]
	var disposition, runID, file, format string
	jsonOutput := false
	for len(arguments) > 0 {
		argument := arguments[0]
		switch argument {
		case "--json":
			jsonOutput = true
			arguments = arguments[1:]
		case "--disposition", "--run", "--file", "--format":
			if len(arguments) < 2 || arguments[1] == "" {
				return usageError(environment.Stderr, fmt.Errorf("%s requires a value", argument))
			}
			value := arguments[1]
			arguments = arguments[2:]
			var target *string
			switch argument {
			case "--disposition":
				target = &disposition
			case "--run":
				target = &runID
			case "--file":
				target = &file
			case "--format":
				target = &format
			}
			if *target != "" {
				return usageError(environment.Stderr, fmt.Errorf("%s may be specified only once", argument))
			}
			*target = value
		case "-h", "--help":
			writeFindingHelp(environment.Stdout)
			return 0
		default:
			return usageError(environment.Stderr, fmt.Errorf("unknown finding close option %q", argument))
		}
	}
	if disposition == "" || file == "" {
		return usageError(environment.Stderr, errors.New("finding close requires --disposition and --file"))
	}
	if format == "" {
		format = "text"
	}
	closureContent, err := readFindingClosureContent(environment.Stdin, file, format)
	if errors.Is(err, store.ErrInvalid) {
		return usageError(environment.Stderr, err)
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	closureID, err := ids.New("closure_")
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	ledger, err := store.Open(ctx, stateDir)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	finding, err := ledger.CloseFinding(ctx, findingID, store.CloseFindingParams{
		ID: closureID, Disposition: store.FindingDisposition(disposition),
		ResolvingRunID: runID, Content: closureContent,
	})
	if errors.Is(err, store.ErrInvalid) {
		return usageError(environment.Stderr, err)
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	if jsonOutput {
		return writeJSON(environment.Stdout, finding, environment.Stderr)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Closed %s (%s)\n", safeHumanText(finding.ID), finding.State)
	if finding.Closure != nil && finding.Closure.ResolvingRunID != "" {
		fmt.Fprintf(&output, "Resolving run: %s\n", safeHumanText(finding.Closure.ResolvingRunID))
	}
	return writeText(environment.Stdout, output.String(), environment.Stderr)
}

func runFindingList(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	jsonOutput := false
	options := store.ListFindingsOptions{}
	for len(arguments) > 0 {
		argument := arguments[0]
		switch argument {
		case "--json":
			jsonOutput = true
			arguments = arguments[1:]
		case "--limit", "--offset", "--state", "--query":
			if len(arguments) < 2 {
				return usageError(environment.Stderr, fmt.Errorf("%s requires a value", argument))
			}
			value := arguments[1]
			arguments = arguments[2:]
			switch argument {
			case "--limit":
				parsed, err := strconv.Atoi(value)
				if err != nil || parsed < 1 || parsed > 1000 {
					return usageError(environment.Stderr, fmt.Errorf("invalid --limit %q", value))
				}
				options.Limit = parsed
			case "--offset":
				parsed, err := strconv.Atoi(value)
				if err != nil {
					return usageError(environment.Stderr, fmt.Errorf("invalid --offset %q", value))
				}
				options.Offset = parsed
			case "--state":
				options.State = store.FindingState(value)
			case "--query":
				options.Query = value
			}
		case "-h", "--help":
			writeFindingHelp(environment.Stdout)
			return 0
		default:
			return usageError(environment.Stderr, fmt.Errorf("unknown finding list option %q", argument))
		}
	}
	if err := store.ValidateListFindingsOptions(options); err != nil {
		return usageError(environment.Stderr, err)
	}
	ledger, err := store.OpenReadOnly(ctx, stateDir)
	if errors.Is(err, store.ErrNotFound) {
		if jsonOutput {
			fmt.Fprintln(environment.Stdout, "[]")
		} else {
			fmt.Fprintln(environment.Stdout, "No findings recorded.")
		}
		return 0
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	findings, err := ledger.ListFindings(ctx, options)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	if jsonOutput {
		return writeJSON(environment.Stdout, findings, environment.Stderr)
	}
	if len(findings) == 0 {
		fmt.Fprintln(environment.Stdout, "No findings recorded.")
		return 0
	}
	writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "FINDING ID\tSTATE\tRECORDED (UTC)\tORIGIN RUN\tSUMMARY")
	for _, finding := range findings {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			safeHumanText(finding.ID), finding.State,
			finding.RecordedAt.UTC().Format(time.RFC3339),
			safeHumanText(finding.OriginRunID), safeHumanText(finding.Content.Summary))
	}
	if err := writer.Flush(); err != nil {
		return commandError(environment.Stderr, fmt.Errorf("write finding list: %w", err))
	}
	return 0
}

type findingView struct {
	Finding      store.Finding `json:"finding"`
	OriginRun    store.Run     `json:"origin_run"`
	ResolvingRun *store.Run    `json:"resolving_run,omitempty"`
}

func runFindingShow(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	jsonOutput := false
	var findingID string
	for _, argument := range arguments {
		switch argument {
		case "--json":
			jsonOutput = true
		case "-h", "--help":
			writeFindingHelp(environment.Stdout)
			return 0
		default:
			if findingID != "" {
				return usageError(environment.Stderr, errors.New("finding show requires one finding ID"))
			}
			findingID = argument
		}
	}
	if findingID == "" {
		return usageError(environment.Stderr, errors.New("finding show requires one finding ID"))
	}
	ledger, err := store.OpenReadOnly(ctx, stateDir)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	finding, err := ledger.GetFinding(ctx, findingID)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	originRun, err := ledger.GetRun(ctx, finding.OriginRunID)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	view := findingView{Finding: finding, OriginRun: originRun}
	if finding.Closure != nil && finding.Closure.ResolvingRunID != "" {
		resolvingRun, err := ledger.GetRun(ctx, finding.Closure.ResolvingRunID)
		if err != nil {
			return commandError(environment.Stderr, err)
		}
		view.ResolvingRun = &resolvingRun
	}
	if jsonOutput {
		return writeJSON(environment.Stdout, view, environment.Stderr)
	}
	return writeText(environment.Stdout, formatFindingView(view), environment.Stderr)
}

func formatFindingView(view findingView) string {
	var destination strings.Builder
	finding := view.Finding
	fmt.Fprintf(&destination, "Finding: %s\n", safeHumanText(finding.ID))
	fmt.Fprintf(&destination, "State: %s\n", finding.State)
	fmt.Fprintf(&destination, "Recorded: %s\n", finding.RecordedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&destination, "Origin run: %s (%s, %s, %s)\n",
		safeHumanText(view.OriginRun.ID), view.OriginRun.Outcome,
		safeHumanText(view.OriginRun.ExecutableBasename), view.OriginRun.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&destination, "Summary: %s\n", safeHumanText(finding.Content.Summary))
	if finding.Content.Hypothesis != "" {
		fmt.Fprintf(&destination, "Hypothesis: %s\n", safeHumanText(finding.Content.Hypothesis))
	}
	if finding.Content.NextStep != "" {
		fmt.Fprintf(&destination, "Next step: %s\n", safeHumanText(finding.Content.NextStep))
	}
	if finding.Closure == nil {
		return destination.String()
	}
	fmt.Fprintf(&destination, "Closed: %s (%s)\n",
		finding.Closure.RecordedAt.UTC().Format(time.RFC3339), finding.Closure.Disposition)
	if view.ResolvingRun != nil {
		fmt.Fprintf(&destination, "Resolving run: %s (%s, %s, %s)\n",
			safeHumanText(view.ResolvingRun.ID), view.ResolvingRun.Outcome,
			safeHumanText(view.ResolvingRun.ExecutableBasename), view.ResolvingRun.StartedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&destination, "Closure note: %s\n", safeHumanText(finding.Closure.Content.Note))
	return destination.String()
}

func readFindingContent(stdin io.Reader, path, format string) (store.FindingContent, error) {
	raw, err := readFindingInput(stdin, path)
	if err != nil {
		return store.FindingContent{}, err
	}
	switch format {
	case "text":
		return store.FindingContent{Summary: normalizeTextInput(raw)}, nil
	case "json":
		return store.ParseFindingContentJSON(raw)
	default:
		return store.FindingContent{}, fmt.Errorf("%w: invalid format %q (want text or json)", store.ErrInvalid, format)
	}
}

func readFindingClosureContent(stdin io.Reader, path, format string) (store.FindingClosureContent, error) {
	raw, err := readFindingInput(stdin, path)
	if err != nil {
		return store.FindingClosureContent{}, err
	}
	switch format {
	case "text":
		return store.FindingClosureContent{Note: normalizeTextInput(raw)}, nil
	case "json":
		return store.ParseFindingClosureContentJSON(raw)
	default:
		return store.FindingClosureContent{}, fmt.Errorf("%w: invalid format %q (want text or json)", store.ErrInvalid, format)
	}
}

func readFindingInput(stdin io.Reader, path string) ([]byte, error) {
	var source io.Reader = stdin
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open finding input %q: %w", path, err)
		}
		defer file.Close()
		source = file
	}
	limited := io.LimitReader(source, store.MaxFindingPayloadBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read finding input: %w", err)
	}
	if len(raw) > store.MaxFindingPayloadBytes {
		return nil, fmt.Errorf("%w: finding input exceeds %d bytes", store.ErrInvalid, store.MaxFindingPayloadBytes)
	}
	return raw, nil
}

func normalizeTextInput(raw []byte) string {
	value := string(raw)
	if strings.HasSuffix(value, "\r\n") {
		return strings.TrimSuffix(value, "\r\n")
	}
	return strings.TrimSuffix(value, "\n")
}

func writeFindingHelp(destination io.Writer) {
	fmt.Fprintln(destination, `Usage:
  motus [--state-dir PATH] finding add --run RUN_ID --file FILE
      [--format text|json] [--json]
  motus [--state-dir PATH] finding list [--state open|resolved|dismissed]
      [--query TEXT] [--limit N] [--offset N] [--json]
  motus [--state-dir PATH] finding show FINDING_ID [--json]
  motus [--state-dir PATH] finding close FINDING_ID
      --disposition resolved --run RUN_ID --file FILE
      [--format text|json] [--json]
  motus [--state-dir PATH] finding close FINDING_ID
      --disposition dismissed --file FILE [--format text|json] [--json]

FILE may be - to read stdin. Text input records one summary when adding and one
closure note when closing. JSON finding input accepts summary, hypothesis, and
next_step. JSON closure input accepts note. Content is stored only when supplied.`)
}
