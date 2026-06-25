# Detect plan-approval prompt as waiting

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

Plan-approval prompt ('…ready to execute. Would you like to proceed?') is not detected as waiting in the hooked-Claude path. permissionPromptSignatures only matches 'do you want to'/'esc to cancel'; the plan box says 'Would you like to proceed?'. When Claude's Notification hook doesn't fire (AskUserQuestion/ExitPlanMode family), the file stays 'working' and the stale-working demotion never triggers. Fix: add 'would you like to proceed' signature + test.
<!-- openkanban:card-notes end -->
