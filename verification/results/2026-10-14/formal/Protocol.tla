------------------------------ MODULE Protocol ------------------------------
EXTENDS Naturals, Sequences, FiniteSets, TLC

\* Canonical transaction abstraction shared by exploration and trace checks.
\* One mailbox, cell and task; ownership and native identity are abstracted.
Initial == [mail |-> <<>>, acked |-> 0, batch |-> 0, revision |-> 0,
            value |-> "null", task |-> FALSE, terminal |-> "pending",
            ledger |-> [o \in {} |-> "none"]]
Minimum(a,b) == IF a < b THEN a ELSE b
Maximum(a,b) == IF a > b THEN a ELSE b
Command(kind,input,expected,limit,sequence) ==
  [kind |-> kind, input |-> input, expected |-> expected,
   limit |-> limit, sequence |-> sequence]
End(s,c) == IF s.batch > s.acked THEN s.batch
            ELSE IF Len(s.mail) > s.acked
                 THEN Minimum(Len(s.mail),s.acked+c.limit) ELSE 0
Allowed(s,c,fresh) ==
  CASE c.kind = "send" -> fresh \notin {s.mail[i] : i \in 1..Len(s.mail)}
    [] c.kind = "cell_cas" -> TRUE
    [] c.kind = "inbox" -> c.limit \in 1..4
    [] c.kind = "ack" -> /\ c.sequence \in 1..Len(s.mail)
                         /\ c.sequence <= s.acked \/ c.sequence <= s.batch
    [] c.kind = "task_create" -> ~s.task
    [] c.kind = "task_finish" ->
         s.task /\ (s.terminal = "pending" \/ s.terminal = c.input)
    [] OTHER -> FALSE
CoreReply(s,c,fresh) ==
  IF ~Allowed(s,c,fresh) THEN [ok |-> FALSE, result |-> "precondition"]
  ELSE [ok |-> TRUE, result |->
    CASE c.kind = "send" -> [message |-> fresh, sequence |-> Len(s.mail)+1]
      [] c.kind = "cell_cas" ->
           [swapped |-> c.expected = s.revision,
            revision |-> IF c.expected = s.revision THEN s.revision+1
                         ELSE s.revision,
            output |-> IF c.expected = s.revision THEN c.input ELSE s.value]
      [] c.kind = "inbox" ->
           [messages |-> SubSeq(s.mail,s.acked+1,
                                Minimum(End(s,c),s.acked+c.limit)),
            acked |-> s.acked, batch |-> End(s,c)]
      [] c.kind = "ack" -> [acked |-> Maximum(s.acked,c.sequence)]
      [] c.kind = "task_create" -> [created |-> TRUE]
      [] c.kind = "task_finish" -> [output |-> c.input]]
CoreNext(s,c,fresh) ==
  IF ~Allowed(s,c,fresh) THEN s
  ELSE CASE c.kind = "send" -> [s EXCEPT !.mail = Append(@,fresh)]
    [] c.kind = "cell_cas" ->
         IF c.expected = s.revision
         THEN [s EXCEPT !.revision = @+1, !.value = c.input] ELSE s
    [] c.kind = "inbox" -> [s EXCEPT !.batch = End(s,c)]
    [] c.kind = "ack" ->
         [s EXCEPT !.acked = Maximum(@,c.sequence),
                   !.batch = IF c.sequence = s.batch THEN 0 ELSE @]
    [] c.kind = "task_create" -> [s EXCEPT !.task = TRUE]
    [] c.kind = "task_finish" -> [s EXCEPT !.terminal = c.input]

\* Effect and replay record are one atomic SQL transaction. Failed commands
\* roll back and do not reserve their operation ID. Replay does not reapply.
Evaluate(s,op,c,fresh) ==
  IF op \in DOMAIN s.ledger THEN
    [state |-> s, reply |-> IF s.ledger[op].command = c
                           THEN s.ledger[op].reply
                           ELSE [ok |-> FALSE, result |-> "arguments"]]
  ELSE LET reply == CoreReply(s,c,fresh)
           next == CoreNext(s,c,fresh)
       IN [state |-> IF reply.ok
                     THEN [next EXCEPT !.ledger =
                            @ @@ (op :> [command |-> c, reply |-> reply])]
                     ELSE s,
           reply |-> reply]
=============================================================================
