package cli

import "fmt"

type checkSeverity uint8

const (
	checkSection checkSeverity = iota
	checkFailure
	checkWarning
	checkOK
	checkInactive
)

type checkFinding struct {
	severity checkSeverity
	message  string
}

type checkReport struct {
	findings []checkFinding
	failures int
	warnings int
}

func (r *checkReport) add(severity checkSeverity, format string, args ...any) {
	r.findings = append(r.findings, checkFinding{severity: severity, message: fmt.Sprintf(format, args...)})
	if severity == checkFailure {
		r.failures++
	}
	if severity == checkWarning {
		r.warnings++
	}
}

func (r *checkReport) section(name string)      { r.add(checkSection, "%s", name) }
func (r *checkReport) failf(f string, a ...any) { r.add(checkFailure, f, a...) }
func (r *checkReport) warnf(f string, a ...any) { r.add(checkWarning, f, a...) }
func (r *checkReport) okf(f string, a ...any)   { r.add(checkOK, f, a...) }
func (r *checkReport) inactivef(f string, a ...any) {
	r.add(checkInactive, f, a...)
}

func (r *checkReport) print() int {
	for _, finding := range r.findings {
		switch finding.severity {
		case checkSection:
			fmt.Println(bold(finding.message))
		case checkFailure:
			fmt.Printf("  %s %s\n", red("✗"), finding.message)
		case checkWarning:
			fmt.Printf("  %s %s\n", warnMark, finding.message)
		case checkOK:
			fmt.Printf("  %s %s\n", green("✓"), finding.message)
		case checkInactive:
			fmt.Printf("  %s %s\n", dim("○"), dim(finding.message))
		}
	}
	fmt.Println()
	if r.failures > 0 {
		fmt.Printf("%s: %d problem(s), %d warning(s)\n", red(bold("check failed")), r.failures, r.warnings)
		return 1
	}
	fmt.Printf("%s (%d warning(s))\n", green(bold("check passed")), r.warnings)
	return 0
}
