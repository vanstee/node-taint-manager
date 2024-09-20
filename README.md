# node-taint-manager

Manage taints on nodes matching the names of required daemonsets. Intended to
be used to prevent pods from being scheduled on nodes that are not yet running
required daemonsets. Similar to how cilium manages a startup taint but without
requiring each daemset to implement it directly.

## Progress

* [x] node and pod informers used to efficiently watch resources
* [x] node taints are successfully removed with a single patch request
* [x] integration test of taint removal using kind
* [ ] breakdown main package into smaller, importable, unit tested packages
* [ ] rework informers to use filtered watch calls
* [ ] use a work queue to trigger reconciliation of specific nodes
* [x] provide public docker image
* [ ] provide public helm chart for installation

## How to use

1. Run node-taint-manager deployment with service account and rbac.

```
kubectl apply -f manifest.yml
kubectl -n node-taint-manager rollout status deployment node-taint-manager
```

2. Configure taints to opt in nodes.

```
taints:
- key: "node.vanstee.github.io/daemonset-not-ready"
  effect: "NoSchedule"
```

3. Configure daemonsets to tolerate any taints.

```
# tolerate all taints
tolerations:
- operator: "Exists"

# ignore all daemonset-not-ready taints
tolerations:
- key: "node.vanstee.github.io/daemonset-not-ready"
  operator: "Exists"
```

4. Ensure daemonset pods are scheduled on nodes as expected and the taints are
   removed once the pods are ready.

## Public image

```
docker pull vanstee/node-taint-manager:latest
```
