sudo rm /var/log/mysql/slow.log
set -e
sudo systemctl restart mysqld
sleep 1
ssh isu1 sudo systemctl restart webapp
ssh isu2 sudo systemctl restart webapp
ssh isu4 sudo systemctl restart webapp
sudo systemctl restart webapp
sudo systemctl restart nginx
wall `echo "\e[95mハローブンブンユーチューブ"`
sleep 5
/home/isucon/go/bin/go-torch --time 70 --url http://localhost:5000/debug/pprof/profile
mv torch.svg /var/www/torch/
cp /var/www/torch/torch.svg /var/www/torch/torch`date +'%H%M'`.svg
sudo mysqldumpslow -s t /var/log/mysql/slow.log > ~/slowlog/time
sudo mysqldumpslow -s l /var/log/mysql/slow.log > ~/slowlog/lock
sudo mysqldumpslow -s c /var/log/mysql/slow.log > ~/slowlog/count
