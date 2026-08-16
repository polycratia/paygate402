package paygate402

import "context"

type payerKey struct{}

// withPayer puts the verified payer on the context.
func withPayer(ctx context.Context, payer string) context.Context {
	if payer == "" {
		return ctx
	}
	return context.WithValue(ctx, payerKey{}, payer)
}

// Payer returns the address the facilitator said paid for this request, if it
// said. A handler behind the gate can use it to meter, to personalise, or to
// refuse — but it is the facilitator's claim, not this package's finding.
func Payer(ctx context.Context) (string, bool) {
	payer, ok := ctx.Value(payerKey{}).(string)
	return payer, ok
}
