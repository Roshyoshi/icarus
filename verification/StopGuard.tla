------------------------------ MODULE StopGuard ------------------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANT TextBoundary
Hooks == {"hook-a","hook-b"}
VARIABLES turn, batch, claimed, fetched, permitted, injections, attempts
vars == <<turn,batch,claimed,fetched,permitted,injections,attempts>>
\* Both turns deliberately have the same assistant text. With no native
\* turn ID, the durable outstanding batch is the fallback boundary.
Boundary(b) == IF TextBoundary THEN "Done" ELSE b
Init == /\ turn = 1 /\ batch = 1 /\ claimed = {}
        /\ fetched = [h \in Hooks |-> 0]
        /\ permitted = [h \in Hooks |-> FALSE]
        /\ injections = [b \in 1..2 |-> 0] /\ attempts = {}
Fetch(h) == /\ fetched' = [fetched EXCEPT ![h] = batch]
            /\ permitted' = [permitted EXCEPT ![h] = FALSE]
            /\ UNCHANGED <<turn,batch,claimed,injections,attempts>>
Claim(h) ==
  /\ fetched[h] > 0
  /\ permitted' = [permitted EXCEPT ![h] = Boundary(fetched[h]) \notin claimed]
  /\ claimed' = claimed \cup {Boundary(fetched[h])}
  /\ attempts' = attempts \cup {fetched[h]}
  /\ UNCHANGED <<turn,batch,fetched,injections>>
Inject(h) ==
  /\ permitted[h]
  /\ injections' = [injections EXCEPT ![fetched[h]] = @+1]
  /\ permitted' = [permitted EXCEPT ![h] = FALSE]
  /\ UNCHANGED <<turn,batch,claimed,fetched,attempts>>
Crash(h) == /\ permitted' = [permitted EXCEPT ![h] = FALSE]
            /\ fetched' = [fetched EXCEPT ![h] = 0]
            /\ UNCHANGED <<turn,batch,claimed,injections,attempts>>
\* Ack plus a later send makes a new outstanding batch. Old fetched output
\* may still be injected; acknowledgement cannot retract a prior fetch.
NextBatch == /\ turn = 1 /\ batch' = 2 /\ turn' = 2
             /\ UNCHANGED <<claimed,fetched,permitted,injections,attempts>>
Next == NextBatch \/ (\E h \in Hooks : Fetch(h) \/ Claim(h) \/ Inject(h) \/ Crash(h))
Spec == Init /\ [][Next]_vars
AtMostOnce == \A b \in 1..2 : injections[b] <= 1
\* Once claimed, the first opportunity for the other batch must remain.
\* A crash after Claim may still prevent injection; no delivery liveness
\* property follows from this durable loop-protection guard.
NewBatchAvailable == batch = 2 /\ 2 \notin attempts =>
                       Boundary(2) \notin claimed
=============================================================================
