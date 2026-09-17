package contract

// FlowNode binds one immutable operation or sealed-plan revision into a DAG.
type FlowNode struct {
	NodeID             string             `json:"node_id"`
	OperationOrPlanRef OperationOrPlanRef `json:"operation_or_plan_ref"`
	DependsOn          []string           `json:"depends_on"`
	ApprovalRef        string             `json:"approval_ref,omitempty"`
}

func (n *FlowNode) UnmarshalJSON(data []byte) error {
	type wire FlowNode
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "node_id", "operation_or_plan_ref", "depends_on"); err != nil {
		return err
	}
	value := FlowNode(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*n = value
	return nil
}

func (n FlowNode) Validate() error {
	if !validIdentityPart(n.NodeID) {
		return contractError(ErrInvalidIdentity, "flow_node.node_id", "must identify one immutable flow node")
	}
	if err := n.OperationOrPlanRef.Validate(); err != nil {
		return err
	}
	if err := validateIdentityList(n.DependsOn, "flow_node.depends_on", true); err != nil {
		return err
	}
	if n.ApprovalRef != "" && !validIdentityPart(n.ApprovalRef) {
		return contractError(ErrInvalidIdentity, "flow_node.approval_ref", "must be canonical when present")
	}
	return nil
}

// FlowSpec is an immutable, explicitly scoped DAG revision.
type FlowSpec struct {
	FlowID      string     `json:"flow_id"`
	OrgID       string     `json:"org_id"`
	WorkspaceID string     `json:"workspace_id"`
	Revision    uint64     `json:"revision"`
	Nodes       []FlowNode `json:"nodes"`
}

func (f *FlowSpec) UnmarshalJSON(data []byte) error {
	type wire FlowSpec
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "flow_id", "org_id", "workspace_id", "revision", "nodes"); err != nil {
		return err
	}
	value := FlowSpec(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*f = value
	return nil
}

func (f FlowSpec) Validate() error {
	if !validIdentityPart(f.FlowID) {
		return contractError(ErrInvalidIdentity, "flow_spec.flow_id", "must identify one immutable flow")
	}
	if err := (Scope{OrgID: f.OrgID, WorkspaceID: f.WorkspaceID}).Validate(); err != nil {
		return err
	}
	if f.Revision == 0 {
		return contractError(ErrInvalidIdentity, "flow_spec.revision", "must be a positive immutable revision")
	}
	if len(f.Nodes) == 0 {
		return contractError(ErrInvalidContract, "flow_spec.nodes", "must contain at least one explicitly declared node")
	}
	nodes := make(map[string]FlowNode, len(f.Nodes))
	for _, node := range f.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
		if _, duplicate := nodes[node.NodeID]; duplicate {
			return contractError(ErrDuplicateField, "flow_spec.nodes.node_id", "flow node identity is duplicated")
		}
		nodes[node.NodeID] = node
	}
	indegree := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))
	for _, node := range f.Nodes {
		for _, dependency := range node.DependsOn {
			if dependency == node.NodeID {
				return contractError(ErrInvalidContract, "flow_spec.nodes.depends_on", "flow node cannot depend on itself")
			}
			if _, exists := nodes[dependency]; !exists {
				return contractError(ErrInvalidIdentity, "flow_spec.nodes.depends_on", "dependency does not name a node in this immutable revision")
			}
			indegree[node.NodeID]++
			dependents[dependency] = append(dependents[dependency], node.NodeID)
		}
	}
	ready := make([]string, 0, len(nodes))
	for id := range nodes {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	visited := 0
	for len(ready) != 0 {
		id := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		visited++
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if visited != len(nodes) {
		return contractError(ErrInvalidContract, "flow_spec.nodes", "dependencies must form an acyclic graph")
	}
	return nil
}
