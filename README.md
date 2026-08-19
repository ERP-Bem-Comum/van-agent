# van-agent

Agente de transporte da VAN Bradesco: liga o **bucket** ao **cliente STCP OFTP** que roda na máquina Windows. É a peça entre "o backend gerou o arquivo de remessa" e "o banco recebeu o arquivo".

Pelo [ADR-0060](https://github.com/ERP-Bem-Comum/core-api/blob/dev/handbook/architecture/adr/0060-van-transport-via-s3-bucket-supersedes-0008-relay.md), a aplicação **nunca** toca a instância da VAN: ela só lê e escreve no bucket. Quem atravessa essa fronteira é este agente.

> ⚠️ **Repositório público.** Nome de bucket, host, OdetteID, número de convênio, caminho de instalação e trechos do manual do fornecedor **não entram aqui**. Tudo isso é configuração de ambiente. O manual do STCP OFTP Client é local-only (restrição de redistribuição) e é citado por **seção e página**, nunca transcrito.

Issue: [core-api#735](https://github.com/ERP-Bem-Comum/core-api/issues/735).

---

## Estado

**Fatia 1 — núcleo, entregue.** O ciclo de transmissão, a idempotência e o tratamento de execução interrompida existem e são exercitados contra um duplo do cliente STCP.

**Fatia 2 — o adapter de object storage, entregue.** O ciclo atravessa a fronteira: `-modo=transmissao` lê e escreve num bucket de verdade.

| Critério                                                                       | Estado     |
| :----------------------------------------------------------------------------- | :--------- |
| **CA1** — transmissão publica status e move para processados                   | ✅         |
| **CA2** — recusa vai para falhas com o código, sem retentativa automática      | ✅         |
| **CA3** — nome já processado **não aciona** o cliente                          | ✅         |
| **CA4** — execução interrompida vai para revisão humana, **nunca** retransmite | ✅         |
| **CA7** — filtro de nomenclatura, inclusive contra arquivo intruso na pasta    | ✅         |
| **CA8** — credencial por role da instância, nada em disco                      | ✅¹        |
| **CA5** — ciclo de recepção                                                    | ✅         |
| **CA6** — erro de identidade não gera retentativa cega                         | ⬜ [#3][i3] |

¹ do lado do código: sem chave informada, a resolução cai na cadeia de provedores, e nada de credencial existe em disco ou em variável. A role **atribuída à instância** é infraestrutura, e não vive neste repositório.

[i3]: https://github.com/ERP-Bem-Comum/van-agent/issues/3

O **cliente STCP continua sendo um duplo** nos testes, e continua sendo pela razão que não muda: não há ambiente de homologação, e a única conexão existente é a de produção no convênio real. O que deixou de ser duplo é o armazenamento — `internal/bucket/s3.go` implementa `bucket.Store` sobre o SDK oficial da AWS, e a suíte o exercita contra um endpoint S3-compatível quando há um configurado.

⚠️ **A fidelidade do duplo do cliente é o teto da confiança de todo o resto.** Ele é modelado a partir do manual, mas não é o binário do banco.

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
go vet ./... && gofmt -l .         # não há linter configurado; use estes
go build ./cmd/van-agent           # o binário
GOOS=windows GOARCH=amd64 go build -o van-agent.exe ./cmd/van-agent   # compilação cruzada
```

### Verificação contínua

`.github/workflows/ci.yml` roda em todo push para `main` e em todo pull request, em dois jobs:

| Job          | O que cobre                                                                                          |
| :----------- | :---------------------------------------------------------------------------------------------------- |
| `verificar`  | `go test ./...` · `go vet ./...` · `gofmt -l .` vazio · compilação cruzada para **windows/amd64** · o golden versionado bate com o que o código produz |
| `integracao` | a mesma suíte com um **MinIO efêmero** de service, para que o adapter de object storage seja exercitado de verdade e não apenas pulado |

**O que o CI deliberadamente NÃO cobre: execução na máquina Windows real.** O binário de produção é windows/amd64, e aqui ele apenas **compila** para esse alvo — toda a suíte roda em Linux. Nada substitui rodar lá, e este agente não tem staging: o que existe é a máquina de produção.

Duas escolhas do arquivo que não são detalhe:

- **Actions de terceiros fixadas por SHA, nunca por tag.** Uma tag é um ponteiro que o dono do repositório pode mover, e mover a tag de uma action que roda no nosso CI é execução de código arbitrário no pipeline de um componente que transmite pagamento. Ao atualizar, trocar o SHA e o comentário da versão juntos.
- **Nenhuma credencial, endpoint ou nome de bucket real no arquivo.** As chaves do job de integração pertencem a um MinIO criado e destruído pelo próprio job; o bucket é criado pelo teste, com nome derivado do relógio.

### Teste de integração do armazenamento

Os testes que falam com um object storage de verdade são **pulados** quando não há endpoint configurado — a suíte precisa continuar rodável numa máquina sem nuvem e sem rede. Para exercitá-los, aponte para qualquer S3-compatível:

```bash
VAN_S3_ENDPOINT=http://<host>:<porta> \
VAN_S3_REGION=us-east-1 \
VAN_S3_ACCESS_KEY_ID=… VAN_S3_SECRET_ACCESS_KEY=… \
VAN_S3_FORCE_PATH_STYLE=true \
go test ./... -count=1
```

Dois testes acordam com isso: `TestCA7_AdapterRealExercitaOsQuatroMetodos` (os quatro métodos da interface) e `TestCA1_CicloCompletoContraObjectStorageReal` (o ciclo inteiro — remessa depositada, transmitida pelo duplo do cliente, status publicado e objeto movido, tudo no bucket real).

### Modo recepção

```bash
van-agent -modo=recepcao
```

Traz o que o banco enviou e o deposita no prefixo de retorno. **É o único ciclo que não paga ninguém se der errado** — transmitir errado tira dinheiro da conta de alguém, receber errado não. Enquanto não houver ambiente de homologação, é por ele que se começa a exercitar qualquer instalação real.

A ordem é **inversa** à da transmissão, e a inversão é deliberada:

```
1. acionar o cliente          (modo R)
2. ler o log do ciclo         ← a evidência de origem
3. listar a pasta de ENTRADA
4. por arquivo: depositar no bucket, DEPOIS registrar
5. publicar o envelope
6. tirar o arquivo da pasta de entrada
```

Na transmissão, registrar antes é o que impede pagar duas vezes. Aqui o risco é o oposto — **perder evidência de um pagamento** —, e registrar antes de depositar abriria exatamente essa janela: uma queda entre o registro e o depósito faria o ciclo seguinte reconhecer o conteúdo como já recebido e nunca depositá-lo.

**Proveniência.** A caixa é do **convênio**, não da nossa remessa: chegam arquivos de lotes que não são nossos. O critério de origem é o **log do ciclo** — as linhas de recepção dizem o que aquela execução recebeu, e vêm do cliente do banco, não de um palpite sobre o nome do arquivo (que é atribuído pelo banco, não por nós). O envelope carrega nome, **SHA-256 do conteúdo**, as linhas cruas que correlacionam e o carimbo, para que o core-api possa aplicar a regra dele: só processa objeto que tenha envelope correspondente; o que aparecer sem envelope vai para quarentena visível.

**O que não casa com o log entra assim mesmo**, marcado como não correlacionado. Erra-se para **mais** aqui, ao contrário da transmissão: descartar em silêncio um arquivo do banco é o desfecho que ninguém percebe.

O agente **nunca abre CNAB** — o conteúdo atravessa cru, byte a byte.

#### Idempotência da recepção

**Nunca sobrescrever. Objeto distinto. Deduplicar por hash de conteúdo, não por nome.**

Sobrescrever um arquivo de retorno destrói evidência de um pagamento — o pior lugar possível para perder registro. E o nome não serve como identificador: quem o atribui é o banco, o mesmo arquivo pode voltar com nome diferente, e nomes iguais podem trazer conteúdo diferente. Deduplicar por nome produziria as **duas falhas opostas** — descartar arquivo novo e aceitar reenvio como novidade.

| O que chega                         | O que acontece                                                                            |
| :---------------------------------- | :---------------------------------------------------------------------------------------- |
| conteúdo **novo**                   | depositado em `retorno/<nome>`, envelope de recepção                                       |
| **mesmo conteúdo** (qualquer nome)  | nada é depositado; envelope declara `duplicado` e aponta `duplicadoDe` para a chave anterior |
| **mesmo nome**, conteúdo diferente  | é arquivo **novo**: vai para uma chave desempatada por carimbo, e os dois ficam recuperáveis |

O índice vive em diretório **próprio** dentro de `VAN_AGENT_LEDGER_DIR` (`recepcao/`): um indexa nome de arquivo de saída, o outro hash de conteúdo de entrada, e uma pasta separada torna a colisão impossível em vez de improvável.

> A idempotência que protege o **negócio** é a do efeito, e ela vive no core-api (chave de negócio: NSA + "Seu Número"). O que o agente garante é outra coisa, e é o que ele pode garantir: que **não perde e não confunde** arquivos. Ele nunca abre CNAB e não conhece chave de negócio alguma.

### Modo transmissão

```bash
van-agent -modo=transmissao
```

Roda o ciclo contra o bucket configurado em `VAN_S3_*`. É o modo de produção.

### Modo ensaio

Verifica uma instalação nova — configuração, pastas, executável, filtro e registro — **sem tocar o bucket**:

```bash
van-agent -modo=ensaio
```

> ⚠️ **O ensaio aciona o cliente STCP de verdade.** Se houver arquivo na pasta de SAÍDA da instalação, ele **será transmitido** — é isso que o cliente faz (§5, p.13). Rodar ensaio numa instalação de produção com a fila suja transmite pagamento real.

### Configuração

Tudo por ambiente; nenhum default aponta para instalação real. São **dois conjuntos**, e a separação tem consequência prática.

**`VAN_AGENT_*` — a máquina.** Lido em qualquer modo, inclusive no ensaio.

| Variável                                                   | Obrigatória | Nota                                   |
| :--------------------------------------------------------- | :---------: | :------------------------------------- |
| `VAN_AGENT_LEDGER_DIR`                                     |     ✅      | caminho **local** e persistente        |
| `VAN_AGENT_NAME_PATTERN`                                   |     ✅      | precisa ancorar o nome inteiro (`^…$`) |
| `VAN_AGENT_STCP_EXE` · `_INI` · `_PROFILE`                 |     ✅      | instalação do cliente                  |
| `VAN_AGENT_STCP_OUTBOUND_DIR` · `_BACKUP_DIR` · `_LOG_DIR` |     ✅      | pastas do cliente                      |
| `VAN_AGENT_STCP_INBOUND_DIR` · `_RECEIVED_DIR`             |   recepção  | pasta de ENTRADA e pasta de arquivados; cobradas **só** no `-modo=recepcao`, para que uma instalação que ainda só transmite continue bootando |
| `VAN_AGENT_NAME_MAX_LENGTH`                                |             | teto de comprimento do nome; **ausente ⇒ sem trava**. Ver abaixo |
| `VAN_AGENT_STCP_RETRIES` · `_RETRY_INTERVAL_SECONDS`       |             | `-r` e `-t` (§6, p.14)                 |
| `VAN_AGENT_STCP_TRANSFER_LOG_GLOB`                         |             | ver pendências                         |

**`VAN_S3_*` — o bucket.** É o **mesmo conjunto que o core-api lê** em `van-s3-config.ts`, com as mesmas regras. Os dois lados leem os mesmos nomes de propósito: um agente que varresse um prefixo e um emissor que depositasse noutro produziriam uma fila silenciosamente vazia, sem erro em lugar nenhum. Exigido só no `-modo=transmissao`.

| Variável                                                                     | Obrigatória | Comportamento no boot                                                    |
| :--------------------------------------------------------------------------- | :---------: | :----------------------------------------------------------------------- |
| `VAN_S3_BUCKET`                                                              |     ✅      | ausente ⇒ falha **nomeando a variável**; homologação e produção são buckets **separados** |
| `VAN_S3_REGION`                                                              |     ✅      | ausente ⇒ falha nomeando a variável                                      |
| `VAN_S3_ENDPOINT`                                                            |             | vazio ⇒ AWS; preenchido ⇒ S3-compatível                                  |
| `VAN_S3_ACCESS_KEY_ID` · `_SECRET_ACCESS_KEY`                                |             | **XOR é erro**: só uma delas ⇒ falha nomeando a que falta. **Ausentes as duas ⇒ cadeia de provedores (role da instância)** — é o caminho de produção |
| `VAN_S3_FORCE_PATH_STYLE`                                                    |             | valor ilegível ⇒ falha. Ver a nota abaixo                                |
| `VAN_S3_PREFIX_OUTBOUND` · `_PROCESSED` · `_FAILED` · `_RETURNS` · `_STATUS` |             | sem barra final ⇒ **normalizado** com barra; começando com barra ⇒ falha nomeando a variável |

#### Teto de comprimento do nome

O manual documenta um erro dedicado a nome longo (**1101**, §11 p.26), e o procedimento que ele descreve é **condicional**: depende de a opção de nome longo estar habilitada na instalação **e** de o parceiro incorporá-la. Nenhuma das duas condições foi verificada por medição — por isso o teto é **configuração**, não constante compilada, e por isso o default é **não travar**: um número fixo no binário congelaria um palpite sobre um acordo bilateral, e um default que recusasse por engano pararia a fila inteira sem que ninguém tivesse pedido.

Com a trava ligada, um nome que a excede vai para o prefixo de falhas com status publicado, **sem que o cliente STCP seja acionado** e sem que o arquivo chegue a ser depositado na pasta de SAÍDA. O nome **não é truncado**: truncar mudaria a chave de idempotência depois de a intenção já estar gravada, e dois nomes distintos truncados para o mesmo prefixo colidiriam no registro — a segunda remessa seria lida como duplicado, **não seria transmitida**, e receberia um envelope com a situação da primeira.

O envelope distingue as duas causas de recusa, porque elas levam a ações diferentes:

| Recusa            | `exitCode` | `logTransferencia` | `detalhe`                                                       |
| :---------------- | :--------- | :----------------- | :-------------------------------------------------------------- |
| **do transporte** | `null`     | vazio              | começa com `[recusa-nomenclatura:padrao]` ou `[recusa-nomenclatura:comprimento]`, e diz que nenhuma tentativa chegou ao banco |
| **do banco**      | preenchido | com as linhas do log | traz o código do §11 na linha crua                              |

Os códigos viajam no `detalhe`, e **não** como campo novo: um campo novo mudaria a forma do envelope, e o contrato do `status/` só muda com as duas metades acordando junto.

**Credencial não é o caminho de produção.** A autenticação é por role da instância (ADR-0061 §5) — nenhuma chave em disco, nenhuma em variável. O par estático existe para exercitar o adapter contra um endpoint local, e o valor **nunca aparece em log**: os tipos que o carregam redigem o segredo ao serem formatados.

**Por que a barra final é erro e não conveniência:** sem ela, `saida` + `ARQUIVO.REM` vira `saidaARQUIVO.REM` — um objeto na raiz do bucket, que nenhuma listagem por prefixo encontra. A remessa sumiria sem erro em lugar nenhum.

> ℹ️ `VAN_S3_FORCE_PATH_STYLE` é **adição deste repositório**, e não existe do lado do core-api. Sem ela, o endereçamento por caminho é ligado pela mesma heurística de lá (endpoint apontando para `localhost`/`127.0.0.1`/`0.0.0.0`). A variável foi necessária ao exercitar o adapter contra um S3-compatível **fora** de localhost: um endereço de rede interna passa pela heurística como se fosse a AWS, e o SDK vai procurar um subdomínio que não existe. Não definida, o comportamento é idêntico ao do core-api — foi por isso que ela pôde entrar sem que a outra metade da fronteira mudasse junto.

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
  bucket/               fronteira com o object storage (interface + duplo + adapter S3)
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
