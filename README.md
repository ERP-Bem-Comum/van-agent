# van-agent

Agente de transporte da VAN Bradesco: liga o **bucket** ao **cliente STCP OFTP** que roda na máquina Windows. É a peça entre "o backend gerou o arquivo de remessa" e "o banco recebeu o arquivo".

Pelo [ADR-0060](https://github.com/ERP-Bem-Comum/core-api/blob/dev/handbook/architecture/adr/0060-van-transport-via-s3-bucket-supersedes-0008-relay.md), a aplicação **nunca** toca a instância da VAN: ela só lê e escreve no bucket. Quem atravessa essa fronteira é este agente.

> ⚠️ **Repositório público.** Nome de bucket, host, OdetteID, número de convênio, caminho de instalação e trechos do manual do fornecedor **não entram aqui**. Tudo isso é configuração de ambiente. O manual do STCP OFTP Client é local-only (restrição de redistribuição) e é citado por **seção e página**, nunca transcrito.

Issue: [core-api#735](https://github.com/ERP-Bem-Comum/core-api/issues/735).

---

## Estado

**Fatia 1 — núcleo, entregue.** O ciclo de transmissão, a idempotência e o tratamento de execução interrompida existem e são exercitados contra um duplo do cliente STCP.

| Critério                                                                       | Estado     |
| :----------------------------------------------------------------------------- | :--------- |
| **CA1** — transmissão publica status e move para processados                   | ✅         |
| **CA2** — recusa vai para falhas com o código, sem retentativa automática      | ✅         |
| **CA3** — nome já processado **não aciona** o cliente                          | ✅         |
| **CA4** — execução interrompida vai para revisão humana, **nunca** retransmite | ✅         |
| **CA7** — filtro de nomenclatura, inclusive contra arquivo intruso na pasta    | ✅         |
| **CA5** — ciclo de recepção                                                    | ⬜ fatia 2 |
| **CA6** — erro de identidade não gera retentativa cega                         | ⬜ fatia 2 |
| **CA8** — credencial por role da instância, nada em disco                      | ⬜ fatia 3 |

O **adapter de object storage não existe ainda** — a fatia 1 se sustenta na interface `bucket.Store` e num duplo em memória. Por isso só o modo `ensaio` está disponível no binário.

---

## O que este agente garante

**Em pagamento, erra-se para menos.** É a regra da qual tudo aqui deriva.

A ordem das operações é a correção deste componente, não preferência de estilo:

```
1. gravar a intenção      ← durável, ANTES de qualquer coisa tocar a pasta de SAÍDA
2. depositar na SAÍDA     ← a partir daqui o cliente pode enviar a qualquer momento (§5, p.13)
3. acionar o cliente
4. ler a evidência física ← sumiu da SAÍDA e apareceu em BACKUP? (ADR-0061 §2)
5. registrar o desfecho
6. publicar o status
7. mover o objeto
```

Inverter 1 e 2 abriria a janela que este componente existe para fechar: uma queda entre depositar e registrar deixaria um arquivo na fila do banco sem que o agente soubesse — e o próximo ciclo, vendo o bucket intacto, depositaria de novo.

O registro de intenção (`internal/ledger`) responde as três perguntas do ciclo:

| Leitura  | Significado                     | Ação                                   |
| :------- | :------------------------------ | :------------------------------------- |
| ausente  | nunca tentamos                  | pode transmitir                        |
| `intent` | tentamos, desfecho desconhecido | **revisão humana**, nunca retransmitir |
| `done`   | já tentamos e sabemos           | duplicado, cliente **não é acionado**  |

**O veredito vem de evidência física, não de código de saída.** O manual v5.3 documenta o resultado no log (§12, p.30), nunca o significado do código de saída do executável — quem depende do código de saída está supondo. O agente decide por o arquivo ter sumido da pasta de SAÍDA e aparecido em BACKUP.

---

## O contrato com o backend

O agente **produz** o que o `core-api` **consome** em `src/modules/financial/adapters/van/status-envelope.ts`. As duas metades são cobradas contra o mesmo arquivo:

```
van-agent/testdata/status-envelope.golden.json
core-api/tests/modules/financial/adapters/van/status-envelope.golden.json   ← cópia literal
```

O golden é **gerado**, nunca escrito à mão:

```bash
go test ./internal/envelope -update
```

Mudou o contrato? Regenere aqui **e** replique no core-api. As duas metades mudam juntas — um golden que descreve algo que o produtor não produz é exatamente a divergência que ele existe para impedir.

Dois detalhes do contrato que quebram o consumidor em silêncio se forem esquecidos:

- `logTransferencia` é **sempre** um array, nunca `null` — o consumidor recusa o envelope inteiro se `Array.isArray` falhar, e um slice nil em Go serializa como `null`;
- `exitCode` é `null` quando o cliente **não foi executado** (caso do duplicado). Trocar por `0` diria "executou e deu certo" — a conclusão oposta.

---

## Rodar

```bash
go test ./...                      # a suíte
go build ./cmd/van-agent           # o binário
GOOS=windows GOARCH=amd64 go build -o van-agent.exe ./cmd/van-agent   # compilação cruzada
```

### Modo ensaio

Verifica uma instalação nova — configuração, pastas, executável, filtro e registro — **sem tocar o bucket**:

```bash
van-agent -modo=ensaio
```

> ⚠️ **O ensaio aciona o cliente STCP de verdade.** Se houver arquivo na pasta de SAÍDA da instalação, ele **será transmitido** — é isso que o cliente faz (§5, p.13). Rodar ensaio numa instalação de produção com a fila suja transmite pagamento real.

### Configuração

Tudo por ambiente; nenhum default aponta para instalação real.

| Variável                                                   | Obrigatória | Nota                                             |
| :--------------------------------------------------------- | :---------: | :----------------------------------------------- |
| `VAN_AGENT_BUCKET`                                         |     ✅      | homologação e produção são buckets **separados** |
| `VAN_AGENT_LEDGER_DIR`                                     |     ✅      | caminho **local** e persistente                  |
| `VAN_AGENT_NAME_PATTERN`                                   |     ✅      | precisa ancorar o nome inteiro (`^…$`)           |
| `VAN_AGENT_STCP_EXE` · `_INI` · `_PROFILE`                 |     ✅      | instalação do cliente                            |
| `VAN_AGENT_STCP_OUTBOUND_DIR` · `_BACKUP_DIR` · `_LOG_DIR` |     ✅      | pastas do cliente                                |
| `VAN_AGENT_STCP_RETRIES` · `_RETRY_INTERVAL_SECONDS`       |             | `-r` e `-t` (§6, p.14)                           |
| `VAN_AGENT_STCP_TRANSFER_LOG_GLOB`                         |             | ver pendências                                   |
| `VAN_AGENT_PREFIX_*`                                       |             | os cinco do ADR-0061 §1                          |

**Credencial não se configura.** A autenticação é por role da instância (ADR-0061 §5) — nenhuma chave em disco, nenhuma em variável.

O agendamento é do **Agendador de Tarefas do Windows** (§10, pp. 20-23), como o fabricante documenta. O processo é one-shot: roda um ciclo e sai. Códigos de saída: `0` OK · `70` erro de execução · `78` configuração.

---

## Pendências que atravessam este componente

Nenhuma se resolve escrevendo código aqui.

1. **Não existe ambiente de homologação.** A conexão validada em 05/08 é a de **produção, no convênio real**, e todos os eventos até hoje foram de recepção — a transmissão nunca foi exercitada. Um arquivo enviado "para testar" vira pagamento de verdade. **É o maior risco deste trabalho.**
2. **A nomenclatura do arquivo de remessa não foi confirmada com o banco** — e o nome não é livre: o banco identifica tipo e fila por ele (ADR-0061, "O que continua em aberto" §1).
3. **O nome do arquivo do log posicional não é documentado.** O manual v5.3 descreve o layout (§12, p.30) e o diretório, mas não o nome; o nome que ele documenta (§7, p.15) é o do log **legível**, que é outro arquivo. Por isso `VAN_AGENT_STCP_TRANSFER_LOG_GLOB` é configuração, com padrão a confirmar contra a instalação.
4. **O dialeto da expressão regular do `-f` não é declarado.** O manual diz que o parâmetro aceita expressão regular (§6, p.14) sem dizer qual. O escape cobre os metacaracteres comuns e ancora o nome inteiro; confirmar contra a instalação é pendência.
5. **O cliente STCP se autoatualiza diariamente** a partir da nuvem do fabricante (§9, p.19). Software mudando sem janela de mudança numa máquina que transmite pagamento — risco conhecido, não nosso a resolver.
6. **O bucket e a máquina não estão versionados como infraestrutura.** Recriar o ambiente hoje depende de conhecimento que não está em repositório nenhum.

### Errata ao ADR-0061

O §4 do ADR-0061 afirma, sobre idempotência: _"Validado por teste… Não há caminho para transmissão dupla."_ **Essa garantia não existia quando o ADR foi aceito** — o agente que a implementaria não havia sido entregue. ADR aceito não se edita; fica o registro de que o §4 devia ser lido como **requisito, não como estado**.

A partir desta fatia, a garantia existe e é testada — em `internal/agent/transmit_test.go`, `TestCA3_NomeJaProcessadoNaoAcionaOCliente` e `TestCA4_ExecucaoInterrompidaVaiParaRevisaoENuncaRetransmite`.

---

## Estrutura

```
cmd/van-agent/          binário one-shot
internal/
  agent/                o ciclo — a ordem das operações
  bucket/               fronteira com o object storage (interface + duplo)
  config/               leitura do ambiente
  envelope/             o contrato do status/
  ledger/               a intenção gravada antes de transmitir
  spool/                pastas do cliente (SAÍDA, BACKUP, LOG) — a evidência física
  stcp/                 linha de comando (§6) e log posicional (§12)
    stcpfake/           duplo do cliente, fiel ao que o manual documenta
testdata/               golden do contrato
```

## Fonte primária

- Manual **BRADESCO STCP OFTP Client v5.3** (06/2023) — local-only em `core-api/handbook/guidelines/bradesco_guideline/van_guide/`. Citado por seção e página.
- [ADR-0060](https://github.com/ERP-Bem-Comum/core-api/blob/dev/handbook/architecture/adr/0060-van-transport-via-s3-bucket-supersedes-0008-relay.md) — a rota (bucket, não SSH).
- [ADR-0061](https://github.com/ERP-Bem-Comum/core-api/blob/dev/handbook/architecture/adr/0061-van-bucket-contract-supersedes-0060-pendencies.md) — o contrato do bucket.
