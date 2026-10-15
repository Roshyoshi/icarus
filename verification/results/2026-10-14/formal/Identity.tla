------------------------------ MODULE Identity ------------------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS AliasProviders, ShallowLineage, StaleRebind
Members == {"main","child-a","sibling-b","grandchild","other-provider"}
Roots == {"main","other-provider"}
Launches == 1..2
Parent(m) == CASE m \in Roots -> ""
              [] m = "grandchild" -> "child-a"
              [] OTHER -> "main"
Ancestors(m) == CASE m \in Roots -> {}
                 [] m = "grandchild" -> {"main","child-a"}
                 [] OTHER -> {"main"}
NativeKey(m) == IF AliasProviders /\ m = "other-provider" THEN "main" ELSE m
VARIABLES generation, parent, binding, badCall
vars == <<generation,parent,binding,badCall>>
Init == /\ generation = [m \in Members |-> 0]
        /\ parent = [m \in Members |-> ""]
        /\ binding = [m \in Members |-> ""] /\ badCall = FALSE
Valid(m,g) == g > 0 /\ generation[m] = g
              /\ \A a \in Ancestors(m) : generation[a] = g
StartMain(m,g) ==
  /\ m \in Roots /\ g > generation[m]
  /\ generation' = [generation EXCEPT ![m] = g]
  /\ binding' = [binding EXCEPT ![m] = NativeKey(m)]
  /\ UNCHANGED <<parent,badCall>>
StartChild(m,g) ==
  /\ m \notin Roots /\ generation[Parent(m)] > 0
  /\ IF StaleRebind THEN g <= generation[Parent(m)]
     ELSE Valid(Parent(m),g) /\ g >= generation[m]
  /\ generation' = [generation EXCEPT ![m] = g]
  /\ parent' = [parent EXCEPT ![m] = Parent(m)]
  /\ binding' = [binding EXCEPT ![m] = NativeKey(m)]
  /\ UNCHANGED badCall
Call(m,g) ==
  /\ g > 0 /\ generation[m] = g
  /\ IF ShallowLineage
     THEN IF Parent(m) = "" THEN TRUE ELSE generation[Parent(m)] = g
     ELSE Valid(m,g)
  /\ badCall' = (badCall \/ ~Valid(m,g))
  /\ UNCHANGED <<generation,parent,binding>>
Next == \E m \in Members, g \in Launches :
          StartMain(m,g) \/ StartChild(m,g) \/ Call(m,g)
Spec == Init /\ [][Next]_vars
Parentage == \A m \in Members : binding[m] # "" => parent[m] = Parent(m)
Distinct == \A m,n \in Members :
  m # n /\ binding[m] # "" /\ binding[n] # "" => binding[m] # binding[n]
Lineage == ~badCall
Monotonic == [][\A m \in Members : generation'[m] >= generation[m]]_vars
=============================================================================
