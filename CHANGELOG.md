# Changelog

Todas as mudanças relevantes deste agente. Formato baseado em
[Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/); versionamento
[semântico](https://semver.org/lang/pt-BR/).

Cada versão publicada tem binários `windows/amd64` anexados ao Release, com o `sha256` nas notas.

## [Não publicado]

## [0.1.2] — 2026-08-27

### Corrigido

- **`logTransferencia` saía `[]` em todo envelope, e `SendOutcome` saía zerado.** O cliente não grava
  o NOME no campo 9 do log posicional: grava o **caminho completo**, e caminhos diferentes para o
  mesmo arquivo conforme a operação — `saida\ARQUIVO.REM` na transmissão,
  `entrada\restart\ARQUIVO.RET.<carimbo>` na recepção em curso, `entrada\ARQUIVO.RET` na concluída.
  A correlação comparava esse campo cru contra o nome e nunca casava, sem que nada emitisse erro.
  (#30)

  `Record.CanonicalName` normaliza em dois passos, e os dois são necessários: sem tirar o caminho
  nada casa; sem tirar o carimbo, as linhas `0006` e `0007` do mesmo retorno viram dois arquivos
  diferentes, e o carimbado — que nunca existe na pasta de entrada — entra em `ReceivedFileNames`
  como um recebido que sumiu antes de alguém olhar. Um alarme que o parser inventaria sozinho.

  Isto é **diagnóstico, não veredito**: a 0.1.1 já publicava a situação correta. O que volta é a
  evidência que acompanha a situação.

### Mudado

- A regra do carimbo saiu de `spool` e passou a `stcp.IsArchiveStamp`. Ela descreve comportamento do
  **cliente** — o mesmo carimbo aparece no campo 9 do log e no nome do arquivo em BACKUP —, então uma
  regra só cobre os dois, em vez de duas cópias que divergem com o tempo. `spool` passou a consumir
  `stcp`; não há ciclo de importação.
- O `stcpfake` escreve o **caminho** no campo 9, e as duas linhas da recepção apontam para lugares
  diferentes, como as 34 recepções medidas na instalação mostram.

### Adicionado

- `van_scripts_notepad/` — pasta local para as revisões do `van.cmd` da máquina do cliente STCP. O
  conteúdo é ignorado pelo git: traz caminhos de instalação, perfil e bucket.

## [0.1.1] — 2026-08-27

### Corrigido

- **`transmitido` era inalcançável nesta instalação, e toda remessa bem-sucedida saía como
  `revisao`.** O cliente não move o arquivo para BACKUP com o mesmo nome: ele **arquiva
  renomeando**, acrescentando o carimbo do instante em que concluiu — `ARQUIVO.REM` vira
  `ARQUIVO.REM.20260827144136918`. O manual não documenta; o §5 (p.13) diz apenas "move para
  backup". Como `spool.InBackup` procurava o nome exato, o arquivo sumia da SAÍDA sem "aparecer" em
  BACKUP e todo desfecho caía no ramo ambíguo de `agent.verdict`. (#29)

  Duas remessas aceitas pelo banco (`Fim de transmissao com sucesso`, resultado `000000`) foram
  publicadas como `revisao` e pararam em `falhas/`. Do outro lado, `revisao` e `falha` colapsam no
  mesmo `Failed`, cuja única saída é o descarte — e o descarte devolve o título para pagamento
  manual de algo que a VAN já pagou.

  O reconhecimento é **estrito, nunca por prefixo**: `.` seguido de ao menos 14 dígitos, com os 14
  primeiros formando uma data plausível. A assimetria de custo manda apertar — um falso negativo
  devolve `revisao`, caro mas seguro; um falso positivo afirma `transmitido` sobre um arquivo que
  não saiu, e aí ninguém reenvia.

### Mudado

- O `stcpfake` passou a carimbar ao arquivar. Ele movia com o nome idêntico, fiel a um manual
  incompleto, e por isso **todo critério de aceite que afirma `transmitido` vinha confirmando uma
  premissa que a instalação real desmente** — suíte verde, produção errada. As asserções de CA que
  faziam `stat` do nome exato eram elas próprias essa premissa; passaram a conferir o disco com
  critério mais frouxo que o da produção, de propósito.

### Notas de operação

- Exige nada da configuração: `van.cmd` não muda entre 0.1.0 e 0.1.1.
- Remessas já publicadas como `revisao` **não são corrigidas** por esta versão. O agente não
  reprocessa o que já saiu de `saida/`.

## [0.1.0] — 2026-08-25

Primeira versão instalada em produção. Transmissão (CA1–CA4, CA7), recepção (CA5/CA6) e credencial
por role (CA8) contra o bucket real; três modos em `cmd/van-agent`.

> ⚠️ **Esta versão não consegue reconhecer uma transmissão bem-sucedida.** Ver 0.1.1. Está aqui pelo
> registro: é a versão que rodou nos ciclos de 26 e 27/08/2026, e saber disso é o que permite
> interpretar os envelopes daqueles dias.

[Não publicado]: https://github.com/ERP-Bem-Comum/van-agent/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/ERP-Bem-Comum/van-agent/releases/tag/v0.1.2
[0.1.1]: https://github.com/ERP-Bem-Comum/van-agent/releases/tag/v0.1.1
[0.1.0]: https://github.com/ERP-Bem-Comum/van-agent/releases/tag/v0.1.0
