# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## O que é

Agente de transporte da VAN Bradesco (Go, stdlib pura, sem dependências externas). Roda na máquina
Windows onde vive o cliente STCP OFTP e é o **único** componente que atravessa a fronteira entre o
bucket e a instalação do banco: a aplicação (`core-api`) nunca toca a VAN, só lê e escreve no bucket
(ADR-0060/0061). Processo **one-shot** — um ciclo e sai; quem agenda é o Agendador de Tarefas do
Windows. Códigos de saída: `0` OK · `70` erro de execução · `78` configuração.

O código lida com **pagamento real e não há ambiente de homologação** — a única conexão existente é
a de produção, no convênio real. A regra da qual tudo deriva: **erra-se para menos**. Na dúvida,
revisão humana; nunca retransmissão automática.

## Comandos

```bash
go test ./...                                   # suíte completa (integração é PULADA sem endpoint)
go test ./internal/agent -run TestCA4_ -v       # um caso (ou um grupo, pelo prefixo CA)
go test ./internal/envelope -update             # regrava o golden do contrato (ver abaixo)
go vet ./... && gofmt -l .                      # não há linter configurado; use estes
go build ./cmd/van-agent
GOOS=windows GOARCH=amd64 go build -o van-agent.exe ./cmd/van-agent
van-agent -modo=ensaio                          # ciclo contra bucket em memória
van-agent -modo=transmissao                     # ciclo contra o bucket real
```

Os testes de integração do armazenamento são **pulados** quando `VAN_S3_ENDPOINT` não está definido.
Para exercitá-los contra um S3-compatível, ver "Teste de integração do armazenamento" no README.

⚠️ **`-modo=ensaio` aciona o cliente STCP de verdade.** Ele roda o ciclo contra um bucket em
memória, mas as pastas e o executável são os reais: se houver arquivo na pasta de SAÍDA, ele **será
transmitido** (§5, p.13). Nunca rodar ensaio em instalação de produção com a fila suja.

## Estado

Fatia 1 (núcleo) entregue: CA1, CA2, CA3, CA4, CA7. Fatia 2 (adapter de object storage) entregue:
`internal/bucket/s3.go` implementa `bucket.Store` sobre o SDK oficial da AWS, e `-modo=transmissao`
roda. CA8 (credencial por role) está atendido do lado do código — sem chave informada, a resolução
cai na cadeia de provedores. Faltam **CA5/CA6** (ciclo de recepção).

O `go.mod` tem as dependências do SDK (`aws-sdk-go-v2/{config,credentials,service/s3}` + `smithy-go`)
e nada além. Elas entram **só** em `internal/bucket/s3.go`: o resto do agente continua stdlib pura, e
a suíte continua rodável sem nuvem e sem rede.

## A ordem das operações é a correção deste componente

`internal/agent/transmit.go` é o coração. A sequência **não é estilo** — não reordene sem entender
o que cada inversão abre:

```
0. republicar pendências  (ReconcilePending) ← desfechos que não chegaram ao bucket antes
1. gravar a intenção      (ledger, fsync)  ← durável, ANTES de tocar a pasta de SAÍDA
2. depositar na SAÍDA     (spool.Place)    ← a partir daqui o cliente pode enviar a qualquer momento
3. acionar o cliente      (stcp.Run)
4. ler a evidência física (sumiu da SAÍDA + apareceu em BACKUP?)
5. registrar o desfecho   (ledger done)
6. publicar o status      (bucket status/) ← pendência gravada ANTES, limpa após confirmar
7. mover o objeto         (processados/ ou falhas/)
```

Inverter 1 e 2 é o bug que este componente existe para impedir: uma queda entre depositar e
registrar deixaria um arquivo na fila do banco sem o agente saber, e o próximo ciclo depositaria de
novo. Inverter 5 e 6 publicaria um desfecho que o registro não conhece, e o arquivo voltaria à fila.

O passo 6 é o único que pode falhar **depois** de o mundo já ter mudado, e o passo 7 roda mesmo
assim — daí a pendência do passo 0. Sem ela, uma falha ao publicar deixava objeto no bucket sem
desfecho, para sempre: o registro já dizia `done` e nada voltava a passar por ali. Vale nos dois
ciclos (`internal/agent/publish.go`), e o corpo republicado é o **original, byte a byte** — o
desfecho não mudou, só a publicação falhou.

Invariantes que decorrem disso:

- **O veredito vem da evidência física, nunca do código de saída.** O manual documenta o resultado
  no log (§12, p.30), não o significado do exit code. `exitCode` é diagnóstico no envelope.
- **Três leituras do ledger:** ausente → pode transmitir · `intent` → revisão humana, **nunca**
  retransmitir (CA4) · `done` → duplicado, cliente **não é acionado** (CA3).
- **Ambíguo é sempre `revisao`, nunca `falha`.** Falha convida alguém a reenviar.
- **Nada é retentado automaticamente** (CA2). Falha e revisão vão para `falhas/` e param ali.
- **`bucket.Store` não tem `Delete`, de propósito** (ADR-0061 §1) — o código que apagaria não
  compila. Estado é localização: mover é a escrita que muda o mundo.
- Erro num objeto não aborta o ciclo (`Summary.Errs` acumula); erro ao **listar** aborta.

## O contrato com o core-api

`internal/envelope` é a metade produtora de um contrato cuja metade consumidora vive em
`core-api/src/modules/financial/adapters/van/status-envelope.ts`. As duas são cobradas contra o
mesmo arquivo:

```
van-agent/testdata/status-envelope.golden.json
core-api/tests/modules/financial/adapters/van/status-envelope.golden.json   ← cópia literal
```

O golden é **gerado, nunca editado à mão** (`go test ./internal/envelope -update`). Mudou o
contrato? Regenere aqui **e** replique no core-api — as duas metades mudam juntas.

Quatro coisas que quebram o consumidor em silêncio:

1. `logTransferencia` é **sempre** array, nunca `null` — slice nil em Go serializa como `null` e o
   consumidor recusa o envelope inteiro. Por isso `envelope.New` é o único construtor exportado.
2. `exitCode` é `*int`: `null` significa "o cliente não foi executado" (caso do duplicado). Trocar
   por `0` diria "executou e deu certo" — a conclusão oposta.
3. As tags JSON são **PT-BR** (`arquivo`, `executadoEm`, `situacao`, `detalhe`, `exitCode`,
   `logTransferencia`) e a chave do duplicado é distinta da normal de propósito — o consumidor
   classifica pela chave. Renomear qualquer um quebra o outro lado.
4. `detalhe` tem teto de **512 caracteres** (`envelope.MaxDetailLength`), garantido em `envelope.New`
   — caracteres, não bytes, porque é assim que o `varchar` do consumidor conta. Sem o teto, um
   detalhe longo derruba o `INSERT` de lá (erro, não truncamento), a confirmação falha, e a varredura
   do core-api **aborta na chave ruim em vez de pulá-la**: toda remessa que ordene depois deixa de ser
   confirmada, para sempre. Quem escolhe **onde** cortar é `agent.resumirErro`, que preserva a
   instrução ao operador e come a cauda do erro do SO — e marca o corte, porque diagnóstico cortado em
   silêncio parece completo.

`Situation` é fechada (`transmitido`/`falha`/`revisao`/`recepcao`); valor fora da lista é recusado
pelo consumidor.

⚠️ **E o contrato não é só o envelope.** Desde 20/08/2026 o core-api também **LÊ os prefixos por nome
de arquivo**: uma rota de download procura o objeto em `saida/` → `processados/` → `falhas/`, nessa
ordem, e para no primeiro que existir (`sandbox/` fica fora de propósito). Ele **só lê** — quem move
continua sendo este agente. Mas isso torna a chave do objeto um dado com prazo de validade, **e o
prazo é nosso**: se o ciclo passar a mover para prefixo novo, ou a renomear ao mover, aquela rota
**para de achar o arquivo em silêncio** — devolve "não encontrado", indistinguível de "arquivo
antigo". Mudar prefixo ou renomear deixou de ser decisão interna deste repositório: avisar antes é o
que impede a rota de passar a mentir.

## Pacotes

| Pacote | Papel |
| :-- | :-- |
| `internal/agent` | o ciclo, a ordem, os critérios de aceite (`FileFilter` monta o `-f`) |
| `internal/bucket` | interface `Store` + `Memory` (duplo, vive em código de produção porque o ensaio o usa) + `S3` (adapter real) |
| `internal/ledger` | intenção em disco local — `O_EXCL` + `Sync` na intenção, tmp+rename na conclusão; nome vira sha256 no caminho. Também o índice de recepção (por hash de conteúdo) e os envelopes pendentes (por chave), cada um em **diretório próprio** |
| `internal/envelope` | contrato do `status/`, chaves e `ValidName` (guarda de fronteira contra `/`, `..` e marcadores) |
| `internal/spool` | pastas SAÍDA/BACKUP/LOG — a evidência física; `Place` escreve fora e renomeia para dentro |
| `internal/stcp` | linha de comando (§6, p.14) e parser do log posicional de 10 campos (§12, p.30) |
| `internal/stcp/stcpfake` | duplo do cliente, fiel ao manual (Succeed/Reject/Vanish/Crash) |
| `cmd/stcp-encenado` | o `stcpfake` como **executável**, para simulação fora da suíte. **Não transmite**, e recusa rodar sem `STCP_ENCENADO_CONFIRMO=nao-transmite-nada` — um falso cliente é perigoso porque parece funcionar |
| `internal/config` | leitura do ambiente; falha no **boot**, nunca no meio de um ciclo |

## Configuração

Dois conjuntos. **`VAN_AGENT_*`** descreve a MÁQUINA (pastas, executável, perfil, ledger,
`NAME_PATTERN`) e é lido em qualquer modo. **`VAN_S3_*`** descreve o BUCKET e é o **mesmo conjunto
que o core-api lê** em `van-s3-config.ts` — os nomes são compartilhados de propósito; renomear de um
lado só quebra a fronteira nos dois sentidos. Só o modo transmissão o exige.

**Nenhum default aponta para instalação real.** `NAME_PATTERN` precisa ancorar o nome inteiro (`^…$`)
— sem âncora a trava não trava. Prefixo sem barra final é **normalizado** (o core-api faz igual);
prefixo começando com barra é **erro**. Homologação e produção são **buckets separados**, não
prefixos. **Credencial não é o caminho de produção**: role da instância (ADR-0061 §5), nada em disco
nem em variável — o par estático existe só para o endpoint de teste, e os tipos que o carregam
redigem o segredo ao serem formatados. Lista completa no README.

`VAN_S3_FORCE_PATH_STYLE` é adição **nossa**, ausente no core-api: sem ela o path-style segue a
heurística de lá (só `localhost`), o que não serve para um S3-compatível em rede interna. Não
definida, o comportamento é idêntico ao do core-api.

## Este repositório é público

Não entram aqui, em código, teste, comentário ou commit: nome de bucket, host, OdetteID, número de
convênio, caminho de instalação, nome de perfil real. `.gitignore` bloqueia `*.pdf`, `*.ini` e
`.env`. O manual do STCP OFTP Client v5.3 é local-only (restrição de redistribuição) e vive no
`core-api` em `handbook/guidelines/bradesco_guideline/van_guide/` — **cite por seção e página, nunca
transcreva**. Nomes em teste são fictícios (`PAG_000000.…`, `PERFIL-DE-TESTE`).

## Convenções

- **Tudo em PT-BR**: comentários, mensagens de erro, nomes de teste, tags JSON. Os comentários
  explicam **por que** a decisão é essa e o que a alternativa quebraria — não o que o código faz.
  Mantenha esse registro ao editar; ele é a documentação do risco.
- Testes de aceite levam o prefixo do critério (`TestCA3_NomeJaProcessadoNaoAcionaOCliente`). O
  `harness` em `internal/agent/transmit_test.go` usa ledger e pastas **reais** em `t.TempDir()` de
  propósito — duplicar o ledger tornaria o teste de morte no meio uma tautologia.
- O que se afirma no CA3 não é o resultado, é que **o cliente não foi acionado** (`fake.Calls()`,
  `Outcome.ClientInvoked`).
- Relógio injetado (`agent.Config.Clock`, `stcpfake.Now`) para carimbos determinísticos.

## Pendências que atravessam o código

Nenhuma se resolve escrevendo Go — ver "Pendências" no README antes de "consertar" o que parece
frouxo: nomenclatura do arquivo de remessa não confirmada com o banco, nome do log posicional não
documentado (daí `STCP_TRANSFER_LOG_GLOB` ser configuração), dialeto da regex do `-f` não declarado
(daí a trava dupla: padrão antes de depositar **e** filtro no acionamento), autoatualização diária
do cliente, e ausência de ambiente de homologação.
