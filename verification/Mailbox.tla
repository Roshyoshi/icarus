------------------------------- MODULE Mailbox -------------------------------
EXTENDS Protocol
CONSTANTS MaxMessages, PrematureAck, StaleSnapshot
Hooks == {"hook-a","hook-b"}
VARIABLES durable, fetched, injected, clientAck, live, phase, hint, observed,
          snapshot, snapshotValid, crashed
vars == <<durable,fetched,injected,clientAck,live,phase,hint,observed,
          snapshot,snapshotValid,crashed>>
Init == /\ durable = Initial
        /\ fetched = [h \in Hooks |-> <<>>] /\ injected = {} /\ clientAck = 0
        /\ live = TRUE /\ phase = "disarmed" /\ hint = FALSE /\ observed = 0
        /\ snapshot = 0 /\ snapshotValid = FALSE /\ crashed = FALSE
CommitSend ==
  /\ Len(durable.mail) < MaxMessages
  /\ durable' = CoreNext(durable,Command("send","body",0,0,0),
                          Len(durable.mail)+1)
  /\ UNCHANGED <<fetched,injected,clientAck,live,phase,hint,observed,
                 snapshot,snapshotValid,crashed>>
Notify == /\ hint' = TRUE
          /\ UNCHANGED <<durable,fetched,injected,clientAck,live,phase,observed,
                         snapshot,snapshotValid,crashed>>
Drop == /\ hint' = FALSE
        /\ UNCHANGED <<durable,fetched,injected,clientAck,live,phase,observed,
                       snapshot,snapshotValid,crashed>>
Arm == /\ live /\ phase = "disarmed" /\ phase' = "read"
       /\ UNCHANGED <<durable,fetched,injected,clientAck,live,hint,observed,
                      snapshot,snapshotValid,crashed>>
Poll ==
  /\ live /\ phase = "read"
  /\ observed' = IF StaleSnapshot /\ snapshotValid THEN snapshot
                  ELSE Len(durable.mail)
  /\ snapshot' = IF snapshotValid THEN snapshot ELSE Len(durable.mail)
  /\ snapshotValid' = TRUE /\ phase' = "sleep"
  /\ UNCHANGED <<durable,fetched,injected,clientAck,live,hint,crashed>>
Tick == /\ live /\ phase = "sleep" /\ phase' = "read"
        /\ UNCHANGED <<durable,fetched,injected,clientAck,live,hint,observed,
                       snapshot,snapshotValid,crashed>>
Wake == /\ live /\ phase = "sleep" /\ hint
        /\ phase' = "read" /\ hint' = FALSE
        /\ UNCHANGED <<durable,fetched,injected,clientAck,live,observed,
                       snapshot,snapshotValid,crashed>>
Fetch(h,n) ==
  /\ live
  /\ LET c == Command("inbox","",0,n,0)
         result == CoreReply(durable,c,0).result
         next == CoreNext(durable,c,0)
     IN /\ fetched' = [fetched EXCEPT ![h] = result.messages]
        /\ durable' = IF PrematureAck /\ result.batch > 0
                       THEN [next EXCEPT !.acked = result.batch, !.batch = 0]
                       ELSE next
  /\ UNCHANGED <<injected,clientAck,live,phase,hint,observed,
                 snapshot,snapshotValid,crashed>>
Inject(h) ==
  /\ live /\ Len(fetched[h]) > 0
  /\ injected' = injected \cup {fetched[h][i] : i \in 1..Len(fetched[h])}
  /\ UNCHANGED <<durable,fetched,clientAck,live,phase,hint,observed,
                 snapshot,snapshotValid,crashed>>
Ack(n) ==
  /\ live /\ Allowed(durable,Command("ack","",0,0,n),0)
  /\ durable' = CoreNext(durable,Command("ack","",0,0,n),0)
  /\ clientAck' = Maximum(clientAck,n)
  /\ UNCHANGED <<fetched,injected,live,phase,hint,observed,
                 snapshot,snapshotValid,crashed>>
Crash == /\ live /\ ~crashed /\ live' = FALSE /\ crashed' = TRUE
         /\ fetched' = [h \in Hooks |-> <<>>] /\ phase' = "disarmed"
         /\ snapshotValid' = FALSE
         /\ UNCHANGED <<durable,injected,clientAck,hint,observed,snapshot>>
Restart == /\ ~live /\ live' = TRUE
           /\ UNCHANGED <<durable,fetched,injected,clientAck,phase,hint,observed,
                          snapshot,snapshotValid,crashed>>
Next == CommitSend \/ Notify \/ Drop \/ Arm \/ Poll \/ Tick \/ Wake
        \/ (\E h \in Hooks, n \in 1..2 : Fetch(h,n))
        \/ (\E h \in Hooks : Inject(h))
        \/ (\E n \in 1..MaxMessages : Ack(n)) \/ Crash \/ Restart
\* Tick abstracts a poll deadline. Weak fairness assumes eventual scheduling
\* and database access. The bounded crash budget ensures stable recovery.
Spec == Init /\ [][Next]_vars /\ WF_vars(Restart) /\ WF_vars(Arm)
        /\ WF_vars(Tick) /\ WF_vars(Poll)
Safety == /\ durable.acked = clientAck
          /\ durable.acked <= Len(durable.mail)
          /\ durable.batch <= Len(durable.mail)
          /\ durable.batch = 0 \/ durable.batch > durable.acked
          /\ observed <= Len(durable.mail)
          /\ \A h \in Hooks :
               /\ Len(fetched[h]) <= 2
               /\ \A i \in 1..Len(fetched[h]) :
                    fetched[h][i] = fetched[h][1]+i-1
          /\ \A i \in 1..Len(durable.mail) : durable.mail[i] = i
OutstandingPrefix == [][durable.batch > durable.acked
  /\ durable'.acked = durable.acked => durable'.batch = durable.batch]_vars
\* This is eventual observation of durable state by a pending waiter, not
\* eventual hook injection or acknowledgement by a model/user.
EventuallyObserve ==
  \A n \in 1..MaxMessages : (Len(durable.mail) >= n) ~> (observed >= n)
=============================================================================
