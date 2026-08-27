# van_scripts_notepad

Cópias locais do `van.cmd` da máquina onde roda o cliente STCP, guardadas aqui para serem
lidas e coladas no Notepad de lá. Uma por revisão, nomeadas por data.

## ⚠️ O conteúdo NUNCA é commitado

Os arquivos desta pasta trazem caminhos de instalação, nome de perfil, nome de bucket e o
padrão do nome de remessa — os mesmos dados que o `.gitignore` já protege em `.env` e
`*.ini`, e que este repositório, sendo **público**, não pode carregar.

O `.gitignore` ignora tudo aqui dentro e abre exceção só para o `.gitkeep` e para este
arquivo:

```
van_scripts_notepad/*
!van_scripts_notepad/.gitkeep
!van_scripts_notepad/LEIA-ME.md
```

Antes de commitar qualquer coisa nesta pasta, confira:

```bash
git status --short van_scripts_notepad/
git check-ignore -v van_scripts_notepad/van.cmd.2026-08-27.txt
```

O primeiro não deve listar nenhum `van.cmd.*`. O segundo deve responder com a regra que o
bloqueia. Se algum arquivo aparecer como *staged*, **não force com `git add -f`** — é
justamente a proteção funcionando.

## Por que `.txt` e não `.cmd`

A extensão é deliberada. Um `.cmd` nesta pasta poderia ser executado por duplo clique ou por
um `for` distraído, e ele **aciona o cliente do banco** — o que, com arquivo na pasta de
SAÍDA, transmite pagamento. Como estes arquivos existem para serem **lidos e copiados**,
nunca executados, `.txt` remove a possibilidade e abre no Notepad do mesmo jeito.

O `van.cmd` que vale é sempre o que está em `C:\van-agent\van.cmd`, na máquina do STCP.
O que está aqui é cópia para leitura, nunca a fonte da verdade.

## Convenção de nome

```
van.cmd.AAAA-MM-DD.txt
```

A data é a da revisão, não a do dia em que foi copiado. Guardar as versões antigas é o que
permite responder "o que mudou entre aquele ciclo e este?" quando um comportamento muda sem
explicação — foi exatamente assim que o comentário errado sobre os prefixos foi encontrado.
