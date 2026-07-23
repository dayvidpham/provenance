package provenance

// activity_plan.go carries the plan-layer signature evolution for the activity
// verbs (roadmap §3.1). Following the repo's additive convention (a plan is
// accepted through an options parameter, matching how optional configuration is
// layered elsewhere without breaking existing call sites), StartActivity and
// StartActivityWithID gained a trailing `opts ...StartActivityOption`. With no
// option the activity is recorded under the built-in "pasture-12-phase" plan (the
// default); InPlan pins a specific plan; Unplanned opts out entirely (nil plan =
// legacy/unplanned).

// StartActivityOption customizes the plan an activity is recorded under.
type StartActivityOption func(*startActivityConfig)

type startActivityConfig struct {
	plan    *PlanID
	planSet bool // an explicit InPlan/Unplanned was chosen; otherwise the default applies
}

// InPlan records the activity as carried out under planID (P-Plan
// correspondsToStep resolves against that plan's steps).
func InPlan(planID PlanID) StartActivityOption {
	return func(c *startActivityConfig) {
		p := planID
		c.plan = &p
		c.planSet = true
	}
}

// Unplanned records the activity with no plan (nil PlanID = legacy/unplanned),
// opting out of the default built-in plan.
func Unplanned() StartActivityOption {
	return func(c *startActivityConfig) {
		c.plan = nil
		c.planSet = true
	}
}

// resolvePlan returns the *PlanID an activity should be recorded under given its
// options: the explicit choice when one was made, otherwise the built-in plan.
func resolvePlan(opts []StartActivityOption) *PlanID {
	var c startActivityConfig
	for _, o := range opts {
		o(&c)
	}
	if c.planSet {
		return c.plan
	}
	builtin := BuiltinPlanID()
	return &builtin
}
