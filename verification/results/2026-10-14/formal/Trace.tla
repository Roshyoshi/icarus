-------------------------------- MODULE Trace --------------------------------
EXTENDS Protocol
CONSTANT Events
VARIABLES cursor, durable, valid
vars == <<cursor,durable,valid>>
Init == /\ cursor = 1 /\ durable = Initial /\ valid = TRUE
Advance ==
  /\ cursor <= Len(Events)
  /\ LET e == Events[cursor]
         result == Evaluate(durable,e.op,e.command,e.fresh)
     IN /\ valid' = (valid /\ e.reply = result.reply)
        /\ durable' = result.state
  /\ cursor' = cursor+1
Spec == Init /\ [][Advance]_vars /\ WF_vars(Advance)
TraceAccepted == valid
TraceConsumed == <> (cursor = Len(Events)+1)
=============================================================================
