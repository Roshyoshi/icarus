---------------------------- MODULE Transactions ----------------------------
EXTENDS Protocol
CONSTANT NonAtomicReplay
Ops == {"send", "cas-a", "cas-b", "finish-a", "finish-b"}
Input(op) == CASE op = "send" -> Command("send","body",0,0,0)
              [] op \in {"cas-a","cas-b"} -> Command("cell_cas",op,0,0,0)
              [] OTHER -> Command("task_finish",op,0,0,0)
VARIABLES durable, effects, available, replies, live, pending
vars == <<durable,effects,available,replies,live,pending>>
Init == /\ durable = [Initial EXCEPT !.task = TRUE]
        /\ effects = [o \in Ops |-> 0]
        /\ available = [o \in Ops |-> [ready |-> FALSE,
                           outcome |-> [ok |-> FALSE,result |-> "none"]]]
        /\ replies = [o \in Ops |-> [ready |-> FALSE,
                           outcome |-> [ok |-> FALSE,result |-> "none"]]]
        /\ live = TRUE /\ pending = ""
Commit(op) ==
  /\ live /\ pending = "" /\ op \notin DOMAIN durable.ledger
  /\ effects[op] < 2
  /\ LET result == Evaluate(durable,op,Input(op),Len(durable.mail)+1) IN
       /\ durable' = IF NonAtomicReplay /\ result.reply.ok
                      THEN [result.state EXCEPT !.ledger = durable.ledger]
                      ELSE result.state
       /\ effects' = IF result.reply.ok
                      THEN [effects EXCEPT ![op] = @+1] ELSE effects
       /\ available' = [available EXCEPT ![op] =
            [ready |-> TRUE,outcome |-> result.reply]]
       /\ pending' = IF NonAtomicReplay /\ result.reply.ok THEN op ELSE ""
  /\ UNCHANGED <<replies,live>>
Record == /\ live /\ pending # ""
          /\ durable' = [durable EXCEPT !.ledger = @ @@
               (pending :> [command |-> Input(pending),
                            reply |-> available[pending].outcome])]
          /\ pending' = ""
          /\ UNCHANGED <<effects,available,replies,live>>
Replay(op) ==
  /\ live /\ op \in DOMAIN durable.ledger
  /\ available' = [available EXCEPT ![op] =
       [ready |-> TRUE,outcome |-> durable.ledger[op].reply]]
  /\ UNCHANGED <<durable,effects,replies,live,pending>>
Reply(op) ==
  /\ live /\ available[op].ready
  /\ pending # op
  /\ replies' = [replies EXCEPT ![op] = available[op]]
  /\ UNCHANGED <<durable,effects,available,live,pending>>
Crash == /\ live /\ live' = FALSE /\ pending' = ""
         /\ available' = [o \in Ops |-> [ready |-> FALSE,
                           outcome |-> [ok |-> FALSE,result |-> "none"]]]
         /\ UNCHANGED <<durable,effects,replies>>
Restart == /\ ~live /\ live' = TRUE
           /\ UNCHANGED <<durable,effects,available,replies,pending>>
Next == (\E o \in Ops : Commit(o) \/ Replay(o) \/ Reply(o))
        \/ Record \/ Crash \/ Restart
Spec == Init /\ [][Next]_vars
Safety == /\ \A o \in Ops : effects[o] <= 1
          /\ durable.revision <= 1
          /\ \A o \in Ops :
               IF ~replies[o].ready THEN TRUE
               ELSE IF replies[o].outcome.ok
                    THEN o \in DOMAIN durable.ledger
                         /\ replies[o].outcome = durable.ledger[o].reply
                    ELSE o \notin DOMAIN durable.ledger
ImmutableResult == [][durable.terminal # "pending" =>
                         durable'.terminal = durable.terminal]_vars
ImmutableReplay == [][\A o \in DOMAIN durable.ledger :
                         durable'.ledger[o] = durable.ledger[o]]_vars
=============================================================================
