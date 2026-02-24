// otel.go initializes the OpenTelemetry tracer for the harness package.
// All harness spans (inference dispatch, CLI execution, HTTP provider calls)
// are recorded under the "cogos-harness" instrumentation scope.
package harness

import "go.opentelemetry.io/otel"

var tracer = otel.Tracer("cogos-harness")
