# Openkanban Goal: Session Preservation

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

One of the overarching goals of Openkanban is to preserve sessions - when a ticket is moved from one status to another, back and forth, keeping the session context is the top goal. Even if I move a ticket to done and then have to pull it back to backlog and promote to in progress again, any time i enter the ticket, the session should still be there. No resets, no new sessions, nada. Exactly one session per ticket, durable. You don't need to do a full audit of the codebase, but just check for any instances where updating ticket status causes a loss of the session context, and change them to preserve the session. Next time I hit enter on that ticket I should be right back where I was
<!-- openkanban:card-notes end -->
