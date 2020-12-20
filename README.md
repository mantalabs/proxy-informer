# proxy-informer

proxy-informer helps run a highly available [Celo
Proxy](https://docs.celo.org/getting-started/mainnet/running-a-validator-in-mainnet#deploy-a-proxy)
on Kubernetes by dynamically discovering Proxies and re-configuring a
specified Validator.

## Usage

Add a proxy-informer container to run as a sidecar to each `Pod`
running a Celo validator container. The proxy-informer sidecar
discovers proxies running on the Kubernetes cluster and makes JSONRPC
requests to update the validator's proxy configuration.

[e2e](./e2e) has an example usage.

### Proxy discovery

Command-line arguments configure the proxy-informer:

| Argument | Default | Description |
| -------- | ------- | ----------- |
| `-proxy-namespace` | `default` | `Namespace` to search for proxy `Service` objects |
| `-proxy-label-selector` | `proxy=true` | [Label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/) to identify `Service` objects |

The proxy-informer discovers internal and external [enode
URL](https://eth.wiki/fundamentals/enode-url-format) values by reading
two annotations on `Service` objects:

| Annotation | Description |
| ---------- | ----------- |
| `proxy.mantalabs.com/internal-enode-url` | Internal enode URL |
| `proxy.mantalabs.com/external-enode-url` | External enode URL |

Annotations can be on two different `Service` objects. For example,
on an internal `Service` for a validator and on a public `Service` for
external traffic.

## Related

* [Kubernetes Handbook Informer Example](https://github.com/feiskyer/kubernetes-handbook/blob/master/examples/client/informer/informer.go)
