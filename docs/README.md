# wintermute documentation

Start with the [README](../README.md) for what wintermute is and how the two
processes fit together. These guides go deeper on the parts that need more than
a paragraph.

| Guide | Read it when |
|---|---|
| [Quickstart](quickstart.md) | You want the shortest path from a clean checkout to renaming a file |
| [Running open-weight models](local-models.md) | You're setting up the machine that serves models — drivers, llama.cpp, quantisation, what fits in 8 GB |
| [Running several backends](backends.md) | You have (or want) more than one model source, and want to know whether that makes anything faster |
| [Hardware reporting from remote hosts](hardware-nodes.md) | You run the server away from the GPUs and want fit estimates back — install the agent with one curl, declare `node` on the backend, and the machine that runs the model is the one graded |

## Where the pieces live

| Question | Answer |
|---|---|
| What can the assistant do on my disks? | Whatever `roots` allows, one approved action at a time — [README, Approval](../README.md#approval) |
| Which model answered that turn? | The turn reports `backend` and `model` — [README](../README.md#choosing-a-model-per-conversation) |
| Will this model fit on my card? | `POST /api/v1/models/fit`, or ask the assistant — it has the same tool |
| Why is it slow? | [backends.md, Measure before you build](backends.md#measure-before-you-build) |
| Why don't tool calls fire? | Almost always `--jinja` — [local-models.md](local-models.md#9-troubleshooting) |

## Conventions in these docs

- Commands prefixed `sudo` run on the **model host**. Commands that talk to
  `localhost:8080` run wherever `wintermuted` is.
- `$TOKEN` is a client token from `wintermuted -add-client`. It is shown once.
- Nothing here should ever require port-forwarding an inference server to the
  internet. If a step seems to, it's wrong.
